package bridge

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"immich-bridge/client"
	"immich-bridge/config"
	"immich-bridge/discovery"
)

// Bridge orchestrates syncing assets between Immich instances.
type Bridge struct {
	config     *config.Config
	clients    map[string]*client.ImmichClient
	dryRun     bool
	ruleFilter string
	noAuto     bool
}

// New creates a Bridge from the loaded config.
func New(cfg *config.Config, dryRun bool, ruleFilter string, noAuto bool) *Bridge {
	clients := make(map[string]*client.ImmichClient, len(cfg.Instances))
	for name, inst := range cfg.Instances {
		clients[name] = client.New(name, inst.URL, inst.APIKey)
	}
	return &Bridge{
		config:     cfg,
		clients:    clients,
		dryRun:     dryRun,
		ruleFilter: ruleFilter,
		noAuto:     noAuto,
	}
}

// Validate pings every configured instance and returns the first failure.
func (b *Bridge) Validate(ctx context.Context) error {
	for name, c := range b.clients {
		if err := c.PingServer(ctx); err != nil {
			return fmt.Errorf("instance %q: %w", name, err)
		}
		slog.Info("instance reachable", "instance", name)
	}
	return nil
}

// DiscoverRules returns the merged set of auto-discovered + explicit rules,
// optionally filtered by --rule and --no-auto flags.
func (b *Bridge) DiscoverRules(ctx context.Context) ([]config.SyncRule, error) {
	var autoRules []config.SyncRule
	if !b.noAuto {
		var err error
		autoRules, err = discovery.DiscoverRules(ctx, b.clients)
		if err != nil {
			return nil, fmt.Errorf("auto-discovery: %w", err)
		}
	}

	rules := discovery.MergeRules(autoRules, b.config.SyncRules)

	if b.ruleFilter != "" {
		var filtered []config.SyncRule
		for _, r := range rules {
			if r.Name == b.ruleFilter {
				filtered = append(filtered, r)
			}
		}
		rules = filtered
	}

	return rules, nil
}

// RunOnce performs a single sync cycle across all rules.
func (b *Bridge) RunOnce(ctx context.Context) error {
	rules, err := b.DiscoverRules(ctx)
	if err != nil {
		return err
	}

	if len(rules) == 0 {
		slog.Info("no sync rules found")
		return nil
	}

	slog.Info("discovered sync rules", "count", len(rules))
	for _, rule := range rules {
		slog.Info("rule",
			"name", rule.Name,
			"source", rule.Source.Instance+"/"+rule.Source.AlbumName,
			"dest", rule.Destination.Instance+"/"+rule.Destination.AlbumName,
			"delete_from_source", rule.ShouldDeleteFromSource())
	}

	for _, rule := range rules {
		if err := b.syncRule(ctx, rule); err != nil {
			slog.Error("sync rule failed", "rule", rule.Name, "error", err)
		}
	}

	return nil
}

// RunDaemon loops RunOnce at the configured poll interval until the context
// is cancelled (e.g. SIGINT/SIGTERM).
func (b *Bridge) RunDaemon(ctx context.Context) error {
	interval := b.config.GetPollDuration()
	slog.Info("starting daemon", "poll_interval", interval)

	for {
		if err := b.RunOnce(ctx); err != nil {
			slog.Error("sync cycle failed", "error", err)
		}

		select {
		case <-ctx.Done():
			slog.Info("daemon shutting down")
			return nil
		case <-time.After(interval):
		}
	}
}

// Status prints all discovered rules and the number of pending assets in each
// source album.
func (b *Bridge) Status(ctx context.Context) error {
	rules, err := b.DiscoverRules(ctx)
	if err != nil {
		return err
	}

	fmt.Printf("Discovered %d sync rules:\n", len(rules))

	for _, r := range rules {
		auto := ""
		if r.AutoDiscovered {
			auto = " [auto-discovered]"
		}
		fmt.Printf("\n  Rule: %s%s\n", r.Name, auto)
		fmt.Printf("    Source:             %s / %s\n", r.Source.Instance, r.Source.AlbumName)
		fmt.Printf("    Destination:        %s / %s\n", r.Destination.Instance, r.Destination.AlbumName)
		fmt.Printf("    Delete from source: %v\n", r.ShouldDeleteFromSource())

		sourceClient := b.clients[r.Source.Instance]
		albumID, err := resolveAlbum(ctx, sourceClient, r.Source.AlbumName)
		if err != nil {
			fmt.Printf("    Pending assets:    (unable to resolve album: %v)\n", err)
			continue
		}
		albumInfo, err := sourceClient.GetAlbumInfo(ctx, albumID)
		if err != nil {
			fmt.Printf("    Pending assets:    (error: %v)\n", err)
			continue
		}
		fmt.Printf("    Pending assets:    %d\n", albumInfo.AssetCount)
	}

	return nil
}

// ---- sync implementation ----

func (b *Bridge) syncRule(ctx context.Context, rule config.SyncRule) error {
	slog.Info("syncing rule", "name", rule.Name)

	sourceClient := b.clients[rule.Source.Instance]
	destClient := b.clients[rule.Destination.Instance]

	// Resolve source album by name
	sourceAlbumID, err := resolveAlbum(ctx, sourceClient, rule.Source.AlbumName)
	if err != nil {
		return fmt.Errorf("resolving source album: %w", err)
	}

	// Resolve or create destination album
	destAlbumID, err := resolveOrCreateAlbum(ctx, destClient, rule.Destination.AlbumName)
	if err != nil {
		return fmt.Errorf("resolving destination album: %w", err)
	}

	// List assets in source album
	sourceAssets, err := sourceClient.GetAlbumAssets(ctx, sourceAlbumID)
	if err != nil {
		return fmt.Errorf("listing source album assets: %w", err)
	}

	if len(sourceAssets) == 0 {
		slog.Info("nothing to sync", "rule", rule.Name)
		return nil
	}

	slog.Info("found assets in source album",
		"rule", rule.Name, "count", len(sourceAssets))

	// Bulk-upload check: which assets already exist on destination?
	checkAssets := make([]client.BulkCheckAsset, len(sourceAssets))
	for i, a := range sourceAssets {
		checkAssets[i] = client.BulkCheckAsset{
			ID:       a.ID,
			Checksum: a.Checksum,
		}
	}

	var needsUpload []string  // source asset IDs that need uploading
	var alreadyOnDest []string // source asset IDs already on destination

	if !b.dryRun {
		bulkResp, err := destClient.CheckBulkUpload(ctx, checkAssets)
		if err != nil {
			return fmt.Errorf("bulk upload check: %w", err)
		}
		for _, result := range bulkResp.Results {
			if result.Action == "accept" {
				needsUpload = append(needsUpload, result.ID)
			} else {
				alreadyOnDest = append(alreadyOnDest, result.ID)
			}
		}
	} else {
		for _, a := range sourceAssets {
			needsUpload = append(needsUpload, a.ID)
		}
	}

	if b.dryRun {
		slog.Info("[DRY RUN] would sync",
			"rule", rule.Name,
			"assets_to_upload", len(needsUpload),
			"already_on_dest", len(alreadyOnDest))
		if rule.ShouldDeleteFromSource() {
			slog.Info("[DRY RUN] would delete from source",
				"rule", rule.Name, "count", len(needsUpload)+len(alreadyOnDest))
		}
		return nil
	}

	// Delete assets already on destination from source immediately
	var deletedCount int
	if rule.ShouldDeleteFromSource() && len(alreadyOnDest) > 0 {
		slog.Info("deleting assets already on destination from source",
			"rule", rule.Name, "count", len(alreadyOnDest))
		if err := sourceClient.DeleteAssets(ctx, alreadyOnDest); err != nil {
			slog.Error("failed to delete already-on-dest assets from source",
				"rule", rule.Name, "error", err)
		} else {
			for _, id := range alreadyOnDest {
				slog.Info("deleted asset from source (already on destination)",
					"asset_id", id, "rule", rule.Name)
			}
			deletedCount += len(alreadyOnDest)
		}
	}

	// Upload new assets — delete each from source immediately after success
	var uploadedCount int
	var uploadErrors int

	for _, assetID := range needsUpload {
		select {
		case <-ctx.Done():
			slog.Warn("sync interrupted", "rule", rule.Name)
			return ctx.Err()
		default:
		}

		if err := b.transferAsset(ctx, sourceClient, destClient, assetID, destAlbumID); err != nil {
			slog.Error("failed to transfer asset",
				"asset_id", assetID, "rule", rule.Name, "error", err)
			uploadErrors++
			continue
		}
		uploadedCount++

		if rule.ShouldDeleteFromSource() {
			if err := sourceClient.DeleteAssets(ctx, []string{assetID}); err != nil {
				slog.Error("failed to delete transferred asset from source",
					"asset_id", assetID, "rule", rule.Name, "error", err)
			} else {
				slog.Info("deleted asset from source",
					"asset_id", assetID, "rule", rule.Name)
				deletedCount++
			}
		}
	}

	slog.Info("sync complete",
		"rule", rule.Name,
		"uploaded", uploadedCount,
		"already_on_dest", len(alreadyOnDest),
		"deleted_from_source", deletedCount,
		"errors", uploadErrors)

	return nil
}

func (b *Bridge) transferAsset(
	ctx context.Context,
	src, dst *client.ImmichClient,
	assetID, destAlbumID string,
) error {
	// 1. Get full metadata from source
	info, err := src.GetAssetInfo(ctx, assetID)
	if err != nil {
		return fmt.Errorf("getting asset info: %w", err)
	}

	// 2. Download to temp file
	body, err := src.DownloadAsset(ctx, assetID)
	if err != nil {
		return fmt.Errorf("downloading asset: %w", err)
	}

	tmpFile, err := os.CreateTemp("", "immich-bridge-*")
	if err != nil {
		body.Close()
		return fmt.Errorf("creating temp file: %w", err)
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath)

	// Hash while writing to verify checksum
	hasher := sha1.New()
	tee := io.TeeReader(body, hasher)

	if _, err := io.Copy(tmpFile, tee); err != nil {
		tmpFile.Close()
		body.Close()
		return fmt.Errorf("writing temp file: %w", err)
	}
	body.Close()
	tmpFile.Close()

	// 3. Verify checksum
	computedHash := hasher.Sum(nil)
	computedHex := hex.EncodeToString(computedHash)
	if info.Checksum != "" {
		if !checksumMatch(info.Checksum, computedHash) {
			return fmt.Errorf("checksum mismatch for %s: immich=%s, computed_sha1=%s",
				info.OriginalFileName, info.Checksum, computedHex)
		}
	}

	// 4. Upload to destination
	uploadFile, err := os.Open(tmpPath)
	if err != nil {
		return fmt.Errorf("opening temp file for upload: %w", err)
	}
	defer uploadFile.Close()

	// x-immich-checksum header expects hex-encoded SHA1
	slog.Debug("uploading asset",
		"file", info.OriginalFileName, "checksum", computedHex)

	uploadResp, err := dst.UploadAsset(ctx,
		info.OriginalFileName, uploadFile, computedHex,
		info.FileCreatedAt, info.FileModifiedAt,
		info.IsFavorite, info.Visibility)
	if err != nil {
		return fmt.Errorf("uploading asset: %w", err)
	}

	slog.Info("uploaded asset",
		"file", info.OriginalFileName,
		"dest_id", uploadResp.ID,
		"status", uploadResp.Status)

	// 5. Set metadata that can't be passed in the upload form
	var dto client.UpdateAssetDTO
	hasUpdate := false

	if info.ExifInfo != nil && info.ExifInfo.Description != "" {
		dto.Description = info.ExifInfo.Description
		hasUpdate = true
	}
	if info.Rating > 0 {
		rating := info.Rating
		dto.Rating = &rating
		hasUpdate = true
	}

	if hasUpdate {
		if err := dst.UpdateAsset(ctx, uploadResp.ID, dto); err != nil {
			slog.Warn("failed to update asset metadata (non-fatal)",
				"asset_id", uploadResp.ID, "error", err)
		}
	}

	// 6. Add to destination album
	if err := dst.AddAssetsToAlbum(ctx, destAlbumID, []string{uploadResp.ID}); err != nil {
		slog.Warn("failed to add asset to destination album (non-fatal)",
			"asset_id", uploadResp.ID, "error", err)
	}

	return nil
}

// ---- album helpers ----

func resolveAlbum(ctx context.Context, c *client.ImmichClient, albumName string) (string, error) {
	albums, err := c.GetAllAlbums(ctx)
	if err != nil {
		return "", err
	}

	var matches []client.Album
	for _, a := range albums {
		if a.AlbumName == albumName {
			matches = append(matches, a)
		}
	}

	if len(matches) == 0 {
		return "", fmt.Errorf("album %q not found on %s", albumName, c.InstanceName())
	}
	if len(matches) > 1 {
		return "", fmt.Errorf("multiple albums named %q on %s (ambiguous)", albumName, c.InstanceName())
	}
	return matches[0].ID, nil
}

func resolveOrCreateAlbum(ctx context.Context, c *client.ImmichClient, albumName string) (string, error) {
	albums, err := c.GetAllAlbums(ctx)
	if err != nil {
		return "", err
	}

	var matches []client.Album
	for _, a := range albums {
		if a.AlbumName == albumName {
			matches = append(matches, a)
		}
	}

	if len(matches) == 1 {
		return matches[0].ID, nil
	}
	if len(matches) > 1 {
		return "", fmt.Errorf("multiple albums named %q on %s (ambiguous)", albumName, c.InstanceName())
	}

	slog.Info("creating destination album",
		"album", albumName, "instance", c.InstanceName())
	album, err := c.CreateAlbum(ctx, albumName)
	if err != nil {
		return "", fmt.Errorf("creating album %q: %w", albumName, err)
	}
	return album.ID, nil
}

// decodeChecksum converts a checksum string (hex or base64) to raw bytes.
// Immich returns checksums as base64-encoded SHA1; we compute hex locally.
func decodeChecksum(s string) ([]byte, error) {
	// Try hex first — 40 hex chars = 20 bytes (SHA1)
	if b, err := hex.DecodeString(s); err == nil {
		return b, nil
	}
	// Try standard base64 (Immich's default encoding)
	if b, err := base64.StdEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	// Try URL-safe base64
	if b, err := base64.URLEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	return nil, fmt.Errorf("cannot decode checksum %q (not hex or base64)", s)
}

// checksumMatch compares an Immich checksum (hex or base64) against raw SHA1 bytes.
func checksumMatch(immichChecksum string, computed []byte) bool {
	decoded, err := decodeChecksum(immichChecksum)
	if err != nil {
		slog.Warn("cannot decode Immich checksum for comparison",
			"checksum", immichChecksum, "error", err)
		return false
	}
	return bytes.Equal(decoded, computed)
}
