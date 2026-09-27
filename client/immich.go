package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strconv"
	"strings"
	"time"
)

const maxRetries = 3

// APIError is returned when the Immich API responds with a non-2xx status.
type APIError struct {
	Method     string
	Path       string
	StatusCode int
	Body       string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("%s %s: status %d: %s", e.Method, e.Path, e.StatusCode, e.Body)
}

func isRetryableStatus(code int) bool {
	return code == 429 || code == 500 || code == 502 || code == 503
}

// ImmichClient wraps HTTP calls to a single Immich instance.
type ImmichClient struct {
	baseURL      string
	apiKey       string
	httpClient   *http.Client
	instanceName string
}

// New creates a client for the given Immich instance.
func New(instanceName, baseURL, apiKey string) *ImmichClient {
	return &ImmichClient{
		baseURL:      strings.TrimRight(baseURL, "/"),
		apiKey:       apiKey,
		httpClient:   &http.Client{Timeout: 5 * time.Minute},
		instanceName: instanceName,
	}
}

// InstanceName returns the human-readable name for this instance.
func (c *ImmichClient) InstanceName() string {
	return c.instanceName
}

// ---- API response/request types ----

type Album struct {
	ID         string `json:"id"`
	AlbumName  string `json:"albumName"`
	AssetCount int    `json:"assetCount"`
}

type AlbumInfo struct {
	ID         string `json:"id"`
	AlbumName  string `json:"albumName"`
	AssetCount int    `json:"assetCount"`
	// Assets is only populated by older Immich servers. Newer releases no
	// longer embed assets in the album response; use GetAlbumAssets instead.
	Assets []AlbumAsset `json:"assets"`
}

type AlbumAsset struct {
	ID       string `json:"id"`
	Checksum string `json:"checksum"`
}

type AssetInfo struct {
	ID               string    `json:"id"`
	Checksum         string    `json:"checksum"`
	OriginalFileName string    `json:"originalFileName"`
	OriginalMimeType string    `json:"originalMimeType"`
	FileCreatedAt    string    `json:"fileCreatedAt"`
	FileModifiedAt   string    `json:"fileModifiedAt"`
	IsFavorite       bool      `json:"isFavorite"`
	Visibility       string    `json:"visibility"`
	Rating           int       `json:"rating"`
	ExifInfo         *ExifInfo `json:"exifInfo,omitempty"`
}

type ExifInfo struct {
	Description string `json:"description,omitempty"`
}

type metadataSearchRequest struct {
	AlbumIDs []string `json:"albumIds"`
	Size     int      `json:"size"`
	Page     int      `json:"page,omitempty"`
	Cursor   string   `json:"cursor,omitempty"`
}

type metadataSearchResponse struct {
	Assets struct {
		Items      []AlbumAsset `json:"items"`
		NextPage   *string      `json:"nextPage"`
		NextCursor *string      `json:"nextCursor"`
	} `json:"assets"`
}

type BulkCheckAsset struct {
	ID       string `json:"id"`
	Checksum string `json:"checksum"`
}

type bulkUploadCheckRequest struct {
	Assets []BulkCheckAsset `json:"assets"`
}

type BulkUploadCheckResponse struct {
	Results []BulkCheckResult `json:"results"`
}

type BulkCheckResult struct {
	ID      string `json:"id"`
	Action  string `json:"action"`
	AssetID string `json:"assetId,omitempty"`
}

type UploadResponse struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

type UpdateAssetDTO struct {
	IsFavorite  *bool  `json:"isFavorite,omitempty"`
	Visibility  string `json:"visibility,omitempty"`
	Description string `json:"description,omitempty"`
	Rating      *int   `json:"rating,omitempty"`
}

type pingResponse struct {
	Res string `json:"res"`
}

// ---- Low-level helpers ----

func (c *ImmichClient) doRequest(ctx context.Context, method, path string, body io.Reader, contentType string) (*http.Response, error) {
	url := c.baseURL + path
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("x-api-key", c.apiKey)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	return c.httpClient.Do(req)
}

// doJSONOnce performs a single JSON round-trip.
func (c *ImmichClient) doJSONOnce(ctx context.Context, method, path string, reqBody, respBody any) error {
	var body io.Reader
	if reqBody != nil {
		data, err := json.Marshal(reqBody)
		if err != nil {
			return fmt.Errorf("marshaling request: %w", err)
		}
		body = bytes.NewReader(data)
	}

	resp, err := c.doRequest(ctx, method, path, body, "application/json")
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respData, _ := io.ReadAll(resp.Body)
		return &APIError{
			Method:     method,
			Path:       path,
			StatusCode: resp.StatusCode,
			Body:       string(respData),
		}
	}

	if respBody != nil {
		if err := json.NewDecoder(resp.Body).Decode(respBody); err != nil {
			return fmt.Errorf("decoding response from %s %s: %w", method, path, err)
		}
	}
	return nil
}

// doJSON performs a JSON round-trip with automatic retry on transient errors.
func (c *ImmichClient) doJSON(ctx context.Context, method, path string, reqBody, respBody any) error {
	var lastErr error
	backoff := time.Second

	for attempt := range maxRetries + 1 {
		lastErr = c.doJSONOnce(ctx, method, path, reqBody, respBody)
		if lastErr == nil {
			return nil
		}

		var apiErr *APIError
		if !errors.As(lastErr, &apiErr) || !isRetryableStatus(apiErr.StatusCode) || attempt == maxRetries {
			return lastErr
		}

		slog.Warn("retrying request",
			"method", method, "path", path,
			"attempt", attempt+1, "backoff", backoff,
			"status", apiErr.StatusCode)

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		backoff *= 2
	}
	return lastErr
}

// ---- Public API methods ----

func (c *ImmichClient) PingServer(ctx context.Context) error {
	var resp pingResponse
	if err := c.doJSON(ctx, http.MethodGet, "/server/ping", nil, &resp); err != nil {
		return err
	}
	if resp.Res != "pong" {
		return fmt.Errorf("unexpected ping response: %q", resp.Res)
	}
	return nil
}

func (c *ImmichClient) GetAllAlbums(ctx context.Context) ([]Album, error) {
	var albums []Album
	if err := c.doJSON(ctx, http.MethodGet, "/albums", nil, &albums); err != nil {
		return nil, err
	}
	return albums, nil
}

func (c *ImmichClient) GetAlbumInfo(ctx context.Context, albumID string) (*AlbumInfo, error) {
	var info AlbumInfo
	if err := c.doJSON(ctx, http.MethodGet, "/albums/"+albumID, nil, &info); err != nil {
		return nil, err
	}
	return &info, nil
}

// GetAlbumAssets returns all assets contained in an album.
//
// Older Immich servers embedded the asset list in GET /albums/{id}; newer ones
// only return metadata (incl. assetCount), so the assets are fetched via
// POST /search/metadata filtered by album ID.
func (c *ImmichClient) GetAlbumAssets(ctx context.Context, albumID string) ([]AlbumAsset, error) {
	info, err := c.GetAlbumInfo(ctx, albumID)
	if err != nil {
		return nil, err
	}
	if len(info.Assets) > 0 {
		return info.Assets, nil
	}
	if info.AssetCount == 0 {
		return nil, nil
	}

	const pageSize = 1000
	var assets []AlbumAsset
	req := metadataSearchRequest{AlbumIDs: []string{albumID}, Size: pageSize, Page: 1}

	for {
		var resp metadataSearchResponse
		if err := c.doJSON(ctx, http.MethodPost, "/search/metadata", req, &resp); err != nil {
			return nil, fmt.Errorf("searching assets of album %s: %w", albumID, err)
		}
		assets = append(assets, resp.Assets.Items...)

		// Safety net: if the server ignored the album filter we would get
		// unrelated assets back, which must never be synced (and deleted).
		if len(assets) > info.AssetCount {
			return nil, fmt.Errorf("album %s: search returned more assets (%d) than the album contains (%d); refusing to continue",
				albumID, len(assets), info.AssetCount)
		}

		switch {
		case resp.Assets.NextCursor != nil && *resp.Assets.NextCursor != "":
			req.Page = 0
			req.Cursor = *resp.Assets.NextCursor
		case resp.Assets.NextPage != nil && *resp.Assets.NextPage != "":
			next, err := strconv.Atoi(*resp.Assets.NextPage)
			if err != nil {
				return nil, fmt.Errorf("invalid nextPage %q: %w", *resp.Assets.NextPage, err)
			}
			req.Cursor = ""
			req.Page = next
		default:
			return assets, nil
		}
	}
}

func (c *ImmichClient) GetAssetInfo(ctx context.Context, assetID string) (*AssetInfo, error) {
	var info AssetInfo
	if err := c.doJSON(ctx, http.MethodGet, "/assets/"+assetID, nil, &info); err != nil {
		return nil, err
	}
	return &info, nil
}

func (c *ImmichClient) DownloadAsset(ctx context.Context, assetID string) (io.ReadCloser, error) {
	resp, err := c.doRequest(ctx, http.MethodGet, "/assets/"+assetID+"/original", nil, "")
	if err != nil {
		return nil, fmt.Errorf("download asset %s: %w", assetID, err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("download asset %s: status %d", assetID, resp.StatusCode)
	}
	return resp.Body, nil
}

func (c *ImmichClient) CheckBulkUpload(ctx context.Context, assets []BulkCheckAsset) (*BulkUploadCheckResponse, error) {
	req := bulkUploadCheckRequest{Assets: assets}
	var resp BulkUploadCheckResponse
	if err := c.doJSON(ctx, http.MethodPost, "/assets/bulk-upload-check", req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// UploadAsset uploads a file to the Immich instance via multipart/form-data.
func (c *ImmichClient) UploadAsset(
	ctx context.Context,
	fileName string,
	fileData io.Reader,
	checksum string,
	fileCreatedAt, fileModifiedAt string,
	isFavorite bool,
	visibility string,
) (*UploadResponse, error) {
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)

	// File part
	partHeader := make(textproto.MIMEHeader)
	partHeader.Set("Content-Disposition",
		fmt.Sprintf(`form-data; name="assetData"; filename="%s"`, fileName))
	partHeader.Set("Content-Type", "application/octet-stream")
	part, err := writer.CreatePart(partHeader)
	if err != nil {
		return nil, fmt.Errorf("creating form file: %w", err)
	}
	if _, err := io.Copy(part, fileData); err != nil {
		return nil, fmt.Errorf("writing file data: %w", err)
	}

	// Required device fields
	writer.WriteField("deviceAssetId", fileName+"-"+checksum)
	writer.WriteField("deviceId", "immich-bridge")

	// Form fields
	writer.WriteField("fileCreatedAt", fileCreatedAt)
	writer.WriteField("fileModifiedAt", fileModifiedAt)
	if isFavorite {
		writer.WriteField("isFavorite", "true")
	} else {
		writer.WriteField("isFavorite", "false")
	}
	if visibility != "" {
		writer.WriteField("visibility", visibility)
	}

	writer.Close()

	url := c.baseURL + "/assets"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, &buf)
	if err != nil {
		return nil, err
	}
	req.Header.Set("x-api-key", c.apiKey)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	if checksum != "" {
		req.Header.Set("x-immich-checksum", checksum)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("upload asset: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		respData, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("upload asset: status %d: %s", resp.StatusCode, string(respData))
	}

	var uploadResp UploadResponse
	if err := json.NewDecoder(resp.Body).Decode(&uploadResp); err != nil {
		return nil, fmt.Errorf("decoding upload response: %w", err)
	}
	return &uploadResp, nil
}

func (c *ImmichClient) CreateAlbum(ctx context.Context, name string) (*Album, error) {
	req := map[string]string{"albumName": name}
	var album Album
	if err := c.doJSON(ctx, http.MethodPost, "/albums", req, &album); err != nil {
		return nil, err
	}
	return &album, nil
}

func (c *ImmichClient) AddAssetsToAlbum(ctx context.Context, albumID string, assetIDs []string) error {
	req := map[string][]string{"ids": assetIDs}
	return c.doJSON(ctx, http.MethodPut, "/albums/"+albumID+"/assets", req, nil)
}

func (c *ImmichClient) UpdateAsset(ctx context.Context, assetID string, dto UpdateAssetDTO) error {
	return c.doJSON(ctx, http.MethodPut, "/assets/"+assetID, dto, nil)
}

func (c *ImmichClient) DeleteAssets(ctx context.Context, assetIDs []string) error {
	req := map[string]any{"ids": assetIDs, "force": true}
	return c.doJSON(ctx, http.MethodDelete, "/assets", req, nil)
}

func (c *ImmichClient) RemoveAssetsFromAlbum(ctx context.Context, albumID string, assetIDs []string) error {
	req := map[string][]string{"ids": assetIDs}
	return c.doJSON(ctx, http.MethodDelete, "/albums/"+albumID+"/assets", req, nil)
}
