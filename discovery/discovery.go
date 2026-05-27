package discovery

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"immich-bridge/client"
	"immich-bridge/config"
)

// DiscoverRules scans every configured instance for albums matching the
// export_<target>_<name> naming convention and returns auto-generated sync rules.
func DiscoverRules(ctx context.Context, instances map[string]*client.ImmichClient) ([]config.SyncRule, error) {
	var rules []config.SyncRule

	for sourceName, sourceClient := range instances {
		albums, err := sourceClient.GetAllAlbums(ctx)
		if err != nil {
			slog.Error("failed to list albums for auto-discovery",
				"instance", sourceName, "error", err)
			continue
		}

		for _, album := range albums {
			rule, ok := parseExportAlbum(sourceName, album.AlbumName, instances)
			if ok {
				rules = append(rules, rule)
			}
		}
	}

	return rules, nil
}

// parseExportAlbum checks whether albumName matches export_<target>_<name>
// and returns a SyncRule if it does.
func parseExportAlbum(sourceName, albumName string, instances map[string]*client.ImmichClient) (config.SyncRule, bool) {
	if !strings.HasPrefix(albumName, "export_") {
		return config.SyncRule{}, false
	}

	// Split into at most 3 parts: "export", target, name
	parts := strings.SplitN(albumName, "_", 3)
	if len(parts) < 3 || parts[2] == "" {
		slog.Warn("malformed export album name (expected export_<target>_<name>)",
			"album", albumName, "instance", sourceName)
		return config.SyncRule{}, false
	}

	target := parts[1]
	name := parts[2]

	// Target must be a configured instance
	if _, ok := instances[target]; !ok {
		slog.Warn("export album references unknown target instance",
			"album", albumName, "target", target, "instance", sourceName)
		return config.SyncRule{}, false
	}

	// No self-loops
	if target == sourceName {
		slog.Warn("export album references self (skipping)",
			"album", albumName, "instance", sourceName)
		return config.SyncRule{}, false
	}

	deleteFromSource := true
	destAlbumName := fmt.Sprintf("import_%s_%s", sourceName, name)

	return config.SyncRule{
		Name: fmt.Sprintf("auto:%s:%s", sourceName, albumName),
		Source: config.SyncRuleEndpoint{
			Instance:  sourceName,
			AlbumName: albumName,
		},
		Destination: config.SyncRuleEndpoint{
			Instance:  target,
			AlbumName: destAlbumName,
		},
		DeleteFromSource: &deleteFromSource,
		AutoDiscovered:   true,
	}, true
}

// MergeRules combines auto-discovered and explicit rules. When an explicit rule
// covers the same source instance+album as an auto-discovered rule, the auto
// rule is dropped (explicit takes precedence).
func MergeRules(autoRules, explicitRules []config.SyncRule) []config.SyncRule {
	explicitKeys := make(map[string]bool, len(explicitRules))
	for _, r := range explicitRules {
		key := r.Source.Instance + "\x00" + r.Source.AlbumName
		explicitKeys[key] = true
	}

	var merged []config.SyncRule
	for _, r := range autoRules {
		key := r.Source.Instance + "\x00" + r.Source.AlbumName
		if explicitKeys[key] {
			slog.Info("auto-discovered rule overridden by explicit rule",
				"source", r.Source.Instance+"/"+r.Source.AlbumName)
			continue
		}
		merged = append(merged, r)
	}

	merged = append(merged, explicitRules...)
	return merged
}
