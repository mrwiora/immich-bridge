# immich-bridge

A Go CLI/service that syncs photos between multiple [Immich](https://immich.app) instances. Name an album `export_<target>_<name>` and the bridge automatically transfers its assets to the target instance — no config changes needed.

**Requires Immich v3.2.0 or newer** on every configured instance (the bridge uses the structured search API). `validate`, `sync`, `daemon` and `status` refuse to run against older servers.

## How it works

```
Source Immich ──► immich-bridge ──► Destination Immich
  (poll albums)     (download)        (upload + metadata)
                    (delete from source after confirmed upload)
```

**No state file.** Successfully uploaded assets are deleted from the source. The source album *is* the queue — if an asset is still there, it hasn't been transferred yet.

## Quick start

1. **Create a config file** (`config.json`):

```json
{
  "instances": {
    "home": {
      "url": "https://immich-home.example.com/api",
      "api_key": "your-home-api-key"
    },
    "backup": {
      "url": "https://immich-backup.example.com/api",
      "api_key": "your-backup-api-key"
    }
  },
  "poll_interval": "5m",
  "log_level": "info"
}
```

2. **Create an album** on your `home` instance named `export_backup_vacation2025`

3. **Run the bridge**:

```bash
# One-shot sync
immich-bridge sync --config config.json

# Or run as a daemon
immich-bridge daemon --config config.json
```

The bridge will:
- Discover `export_backup_vacation2025` on `home`
- Create `import_home_vacation2025` on `backup` (if it doesn't exist)
- Transfer all assets with full metadata
- Delete the transferred assets from `home`

## Convention-based auto-discovery

Any album matching `export_<target>_<name>` is automatically synced:

| Album on instance | Syncs to | Destination album |
|---|---|---|
| `export_backup_test` on **home** | **backup** | `import_home_test` |
| `export_home_shared` on **backup** | **home** | `import_backup_shared` |
| `export_archive_family` on **home** | **archive** | `import_home_family` |

Rules:
- `<target>` must match a configured instance name
- `<name>` is everything after the second underscore (may contain underscores)
- Auto-discovered rules always delete from source after transfer
- Destination albums are auto-created if missing

## Explicit sync rules

For transfers that don't fit the naming convention (custom album names, `delete_from_source: false`), add explicit rules:

```json
{
  "sync_rules": [
    {
      "name": "family-photos-mirror",
      "source": {
        "instance": "home",
        "album_name": "Family Photos"
      },
      "destination": {
        "instance": "backup",
        "album_name": "Family Photos Mirror"
      },
      "delete_from_source": false
    }
  ]
}
```

Explicit rules are processed **in addition to** auto-discovered rules. If both cover the same source album, the explicit rule wins.

## CLI reference

```
immich-bridge <command> [flags]

Commands:
  sync       Run a one-time sync for all rules
  daemon     Run continuously, polling at the configured interval
  validate   Validate config, test API connectivity, list discovered rules
  status     Show discovered rules and pending asset counts

Flags:
  --config string    Path to config file (default "./config.json")
  --dry-run          Show what would be synced/deleted without acting
  --rule string      Run only a specific sync rule by name
  --no-auto          Disable auto-discovery, use only explicit sync_rules
  --verbose          Enable debug logging
```

## Metadata preserved

| Field | How |
|---|---|
| Original file (full quality) | Downloaded via `/original`, uploaded as-is |
| EXIF data (GPS, camera, dates) | Embedded in the file |
| `fileCreatedAt` / `fileModifiedAt` | Passed explicitly in upload |
| `isFavorite` | Set in upload + update |
| `visibility` (archive/timeline/hidden) | Passed in upload |
| `description` | Set via asset update after upload |
| `rating` | Set via asset update after upload |
| Album membership | Asset added to destination album |

**Not transferred:** Tags (instance-specific IDs), People/Faces (ML-generated, re-detected on destination).

## Deployment

### Docker

```bash
docker build -t immich-bridge .
docker run -v ./config.json:/config.json:ro immich-bridge daemon --config /config.json
```

### docker-compose

```yaml
services:
  immich-bridge:
    build: .
    volumes:
      - ./config.json:/config.json:ro
    command: ["daemon", "--config", "/config.json"]
    restart: unless-stopped
```

### systemd

```ini
[Unit]
Description=Immich Bridge - Multi-instance photo sync
After=network.target

[Service]
ExecStart=/usr/local/bin/immich-bridge daemon --config /etc/immich-bridge/config.json
Restart=on-failure
RestartSec=30

[Install]
WantedBy=multi-user.target
```

## Building from source

```bash
go build -o immich-bridge .
```

Zero external dependencies — everything is Go stdlib.

## Error handling

- **Per-asset errors don't stop the batch** — failed assets stay on source and are retried next cycle
- **Deletion is batched** — only after all uploads for a rule succeed
- **Automatic retry** with exponential backoff on HTTP 429/500/502/503
- **Checksum verification** — SHA1 checked after download, before upload
- **Graceful shutdown** — SIGINT/SIGTERM finishes the current asset, skips deletion, exits cleanly

## Configuration reference

| Field | Type | Default | Description |
|---|---|---|---|
| `instances` | object | (required) | Map of instance name → `{url, api_key}` |
| `sync_rules` | array | `[]` | Optional explicit sync rules |
| `poll_interval` | string | `"5m"` | Go duration string for daemon polling |
| `log_level` | string | `"info"` | `debug`, `info`, `warn`, or `error` |

### Sync rule fields

| Field | Type | Default | Description |
|---|---|---|---|
| `name` | string | auto-generated | Human-readable rule identifier |
| `source.instance` | string | (required) | Source instance name |
| `source.album_name` | string | (required) | Source album name |
| `destination.instance` | string | (required) | Destination instance name |
| `destination.album_name` | string | (required) | Destination album name |
| `delete_from_source` | bool | `true` | Delete assets from source after transfer |
