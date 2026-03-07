# ProxS3

Native S3 storage plugin for Proxmox VE. Use any S3-compatible object store as a Proxmox storage backend for ISO images, container templates, snippets, and backups.

Unlike FUSE-based approaches (s3fs, rclone mount), ProxS3 integrates directly with Proxmox's storage subsystem. Proxmox understands the storage is remote, handles disconnects gracefully, and shows proper status in the web UI.

## How It Works

ProxS3 has two components:

- **proxs3d** — a Go daemon that handles all S3 operations, maintains a local file cache, and monitors connectivity. It listens on a Unix socket.
- **S3Plugin.pm** — a Perl storage plugin that registers a native `s3` storage type in Proxmox. All storage operations are forwarded to the daemon.

```
Proxmox UI / pvesm CLI
       |
       v
  S3Plugin.pm  (PVE::Storage::Custom)
       | Unix socket
       v
    proxs3d    (Go daemon)
       | HTTPS              | local disk
       v                    v
   S3 bucket            file cache
  (source of truth)   (per-node, validated)
```

The daemon auto-discovers S3 storages from `/etc/pve/storage.cfg`. When you add, update, or remove an S3 storage via the Proxmox UI or `pvesm`, the plugin signals the daemon to reload automatically.

## Requirements

- Proxmox VE 8.x or 9.x (Debian Bookworm or Trixie)
- An S3-compatible object store (AWS S3, MinIO, Ceph RGW, Cloudflare R2, Wasabi, etc.)
- Network access from each Proxmox node to the S3 endpoint (direct or via HTTP proxy)

## Quick Start

This gets you from zero to a working S3-backed ISO/template store in under 5 minutes.

### Step 1: Install the package

On **each Proxmox node** in your cluster:

```bash
# Download the .deb from the latest release
wget https://github.com/sol1/proxs3/releases/latest/download/proxs3_0.1.0-1_amd64.deb

# Install
dpkg -i proxs3_0.1.0-1_amd64.deb
```

This installs:
- `/usr/bin/proxs3d` — the Go daemon
- `/usr/share/perl5/PVE/Storage/Custom/S3Plugin.pm` — the Proxmox plugin
- `/usr/share/pve-manager/js/s3storage.js` — web UI panel for the S3 storage type
- `/lib/systemd/system/proxs3d.service` — systemd unit
- `/etc/proxs3/proxs3d.json` — daemon config (only created if not already present)

**Important:** After installing or upgrading the package, you must restart the PVE services so they load the new plugin code:

```bash
systemctl restart pvedaemon pveproxy pvestatd
```

These services load the storage plugin at startup. Without a restart, the S3 storage type won't appear in the UI, storage status will show as "unknown", and content type changes won't take effect. The `proxs3d` daemon is managed separately and is restarted automatically by the package.

### Step 2: Configure the daemon

Edit `/etc/proxs3/proxs3d.json` on each node:

```json
{
    "socket_path": "/run/proxs3d.sock",
    "cache_dir": "/var/cache/proxs3",
    "cache_max_mb": 4096,
    "credential_dir": "/etc/pve/priv/proxs3",
    "storage_cfg": "/etc/pve/storage.cfg",
    "headroom_gb": 100
}
```

The only setting you're likely to change is **`cache_dir`** and **`cache_max_mb`**.

**Important: choose your cache location carefully.** ISOs and templates are large files. The cache should live on a disk with plenty of free space, not on a small rootfs. Good choices:

- A dedicated local disk or partition (e.g., `/mnt/proxs3-cache`)
- A fast SSD with spare capacity
- An LVM volume sized for your expected workload

The `cache_max_mb` setting controls the maximum cache size in megabytes. When exceeded, the least recently used files are evicted. Set this to roughly 80% of the available space on your cache disk.

This config file is **per-node** (it's in `/etc/proxs3/`, not `/etc/pve/`) so you can set different cache paths and sizes on different nodes.

### Step 3: Prepare your S3 bucket

Create a bucket in your S3-compatible store and set up the expected directory structure. ProxS3 uses these key prefixes:

| Content Type | S3 Prefix | Example Key |
|---|---|---|
| ISO images | `template/iso/` | `template/iso/debian-12.7-amd64-netinst.iso` |
| Container templates | `template/cache/` | `template/cache/debian-12-standard_12.2-1_amd64.tar.zst` |
| Snippets | `snippets/` | `snippets/cloud-init-user.yaml` |
| Backups | `dump/` | `dump/vzdump-qemu-100-2024_01_01.vma.zst` |

You can create these prefixes by uploading a file to each path, or by creating "folders" in the S3 console. Most S3-compatible stores create prefixes implicitly when you upload objects.

To pre-populate with ISOs, simply upload them to the `template/iso/` prefix:

```bash
aws s3 cp debian-12.7-amd64-netinst.iso s3://my-bucket/template/iso/
```

### Step 4: Start the daemon

```bash
systemctl enable --now proxs3d
```

Check that it started correctly:

```bash
systemctl status proxs3d
journalctl -u proxs3d --no-pager -n 20
```

You should see log lines showing the socket path and the number of discovered storages (zero at this point, since we haven't added one yet).

### Step 5: Add the storage in Proxmox

You only need to do this once per cluster (the config is shared across all nodes via pmxcfs).

**Option A: Via the web UI**

Go to **Datacenter -> Storage -> Add -> S3** and fill in the fields.

**Option B: Via the command line**

```bash
pvesm add s3 my-s3-store \
    --endpoint s3.amazonaws.com \
    --bucket my-proxmox-bucket \
    --region us-east-1 \
    --access-key AKIAIOSFODNN7EXAMPLE \
    --secret-key wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY \
    --content iso,vztmpl,snippets \
    --use-ssl 1
```

For MinIO or other S3-compatible stores that use path-style URLs, add `--path-style 1`.

**What happens behind the scenes:**

1. The plugin writes your credentials to `/etc/pve/priv/proxs3/my-s3-store.json` (root-only, 0600). This file is cluster-shared via pmxcfs.
2. The storage configuration (endpoint, bucket, region, etc.) is written to `/etc/pve/storage.cfg` — but **credentials are not stored there**.
3. The plugin signals proxs3d to reload its configuration.
4. The daemon re-reads `storage.cfg`, discovers the new S3 storage, loads its credentials, and starts health-checking the endpoint.

### Step 6: Verify

```bash
# Check storage status
pvesm status

# List contents
pvesm list my-s3-store

# Or check the web UI — your S3 storage should appear with a green status indicator
```

You should now see any ISOs or templates you uploaded to the bucket. You can upload new ones via the Proxmox UI or download ISOs directly from the storage view.

## Configuration Reference

### Daemon config (`/etc/proxs3/proxs3d.json`)

| Field | Default | Description |
|---|---|---|
| `socket_path` | `/run/proxs3d.sock` | Unix socket path for plugin communication |
| `cache_dir` | `/var/cache/proxs3` | Local cache directory for downloaded objects |
| `cache_max_mb` | `4096` | Maximum cache size in MB before LRU eviction |
| `credential_dir` | `/etc/pve/priv/proxs3` | Directory containing per-storage credential files |
| `storage_cfg` | `/etc/pve/storage.cfg` | Path to Proxmox storage configuration |
| `headroom_gb` | `100` | Available space (in GiB) to report to Proxmox. S3 has no real capacity limit, so this is the "free space" PVE always sees. Set to match what makes sense for your environment. |
| `proxy.http_proxy` | _(empty)_ | HTTP proxy URL for outbound connections |
| `proxy.https_proxy` | _(empty)_ | HTTPS proxy URL for outbound connections |
| `proxy.no_proxy` | _(empty)_ | Comma-separated list of hosts to bypass proxy |

### Storage config (via `pvesm` or web UI)

| Field | Required | Description |
|---|---|---|
| `endpoint` | Yes | S3 endpoint hostname (e.g., `s3.amazonaws.com`, `minio.local:9000`) |
| `bucket` | Yes | S3 bucket name |
| `region` | No | S3 region (defaults to `us-east-1`) |
| `access-key` | Yes | S3 access key ID |
| `secret-key` | Yes | S3 secret access key |
| `content` | No | Comma-separated content types: `iso`, `vztmpl`, `snippets`, `backup` |
| `use-ssl` | No | Use HTTPS (`1`) or HTTP (`0`). Defaults to on. |
| `path-style` | No | Use path-style URLs (`1`) instead of virtual-hosted-style (`0`). Required for MinIO. |
| `nodes` | No | Restrict storage to specific cluster nodes |

### HTTP Proxy

If your Proxmox nodes access the internet through an HTTP proxy, configure it in the daemon config:

```json
{
    "proxy": {
        "https_proxy": "http://proxy.internal:3128",
        "http_proxy": "http://proxy.internal:3128",
        "no_proxy": "localhost,127.0.0.1,.internal"
    }
}
```

The proxy settings apply to all outbound S3 connections from the daemon.

## S3-Compatible Stores

ProxS3 works with any S3-compatible object store. Here are the recommended settings for common providers:

| Provider | `endpoint` | `use-ssl` | `path-style` |
|---|---|---|---|
| AWS S3 | `s3.amazonaws.com` | `1` | `0` |
| AWS S3 (regional) | `s3.us-west-2.amazonaws.com` | `1` | `0` |
| MinIO | `minio.local:9000` | depends | `1` |
| Ceph RGW | `rgw.local:7480` | depends | `1` |
| Cloudflare R2 | `<account-id>.r2.cloudflarestorage.com` | `1` | `0` |
| Wasabi | `s3.wasabisys.com` | `1` | `0` |
| Backblaze B2 | `s3.us-west-004.backblazeb2.com` | `1` | `0` |
| DigitalOcean Spaces | `<region>.digitaloceanspaces.com` | `1` | `0` |

## Caching

The cache is critical to how ProxS3 works. Without it, every file access would require a full download from S3.

**How it works:**

1. When Proxmox requests a file (e.g., to boot a VM from an ISO), the daemon first checks the local cache.
2. If the file is cached, the daemon does a lightweight `HeadObject` call to S3 to check the ETag and LastModified timestamp.
3. If the cached copy matches (ETag is identical), the local path is returned immediately. No download needed.
4. If the remote object has changed (different ETag), the cached copy is invalidated and the new version is downloaded.
5. If the file is not cached at all, it's downloaded from S3 and stored in the cache.

**This means S3 is always the source of truth.** If you update an ISO in your S3 bucket, every node will pick up the change on next access.

**Cache eviction:** When the total cache size exceeds `cache_max_mb`, the oldest files (by modification time) are automatically evicted to make room. This runs asynchronously after each new file is cached.

**Upload caching:** When you upload a file via the Proxmox UI, it's sent to S3 and simultaneously cached locally. This means the file is available for immediate use without waiting for a download.

## Multi-Node Clusters

ProxS3 is designed for Proxmox clusters where you want every node to have access to the same ISOs and templates:

- **Add the storage once** — `storage.cfg` is shared across all nodes via pmxcfs. After `pvesm add`, every node sees the storage.
- **Credentials are cluster-shared** — stored in `/etc/pve/priv/proxs3/`, which is also distributed by pmxcfs. Root-only permissions (0600).
- **Install the .deb on each node** — the daemon and plugin must be present on every node that needs access.
- **Cache is per-node** — each node maintains its own local cache. This is intentional: nodes pull from S3 independently and validate their cache against S3 metadata.
- **Daemon config is per-node** — `/etc/proxs3/proxs3d.json` is local to each node. This lets you set different cache paths and sizes based on each node's local disk layout.

## Daemon Management

```bash
# Start/stop
systemctl start proxs3d
systemctl stop proxs3d

# Reload config (re-reads storage.cfg and credentials, picks up new/changed/removed storages)
systemctl reload proxs3d

# View logs
journalctl -u proxs3d -f

# Check health
systemctl status proxs3d
```

The daemon performs health checks against each configured S3 endpoint every 30 seconds. If an endpoint becomes unreachable, the storage is marked as offline in Proxmox. When connectivity is restored, it's automatically marked as online again.

## Troubleshooting

### S3 doesn't appear in the "Add Storage" dropdown / storage shows grey question mark

The PVE services (`pvedaemon`, `pveproxy`, `pvestatd`) load storage plugins at startup. If you installed or upgraded ProxS3 without restarting them, they won't know about the S3 type:

```bash
systemctl restart pvedaemon pveproxy pvestatd
```

Then hard-refresh your browser (Ctrl+Shift+R). This is the most common issue after install or upgrade.

### Storage shows as "unavailable" in the UI

1. Check the daemon is running: `systemctl status proxs3d`
2. Check the logs: `journalctl -u proxs3d -f`
3. Verify S3 connectivity from the node: `curl -I https://your-endpoint/your-bucket`
4. Verify credentials: `cat /etc/pve/priv/proxs3/<storeid>.json`

### "Cannot connect to proxs3d" errors

The daemon isn't running or the socket doesn't exist:

```bash
systemctl restart proxs3d
ls -la /run/proxs3d.sock
```

### Stale files / wrong version of an ISO

The cache validates against S3 on every access via ETag. If you're seeing stale data:

1. The S3 provider may not be returning proper ETags (some providers don't for multipart uploads)
2. You can clear the cache manually: `rm -rf /var/cache/proxs3/<storeid>/`
3. Restart the daemon: `systemctl restart proxs3d`

### Cache filling up the disk

Set `cache_max_mb` in `/etc/proxs3/proxs3d.json` to limit the cache size. Move `cache_dir` to a disk with more space if needed. After changing, restart the daemon.

### Proxy not working

Verify the proxy settings in `/etc/proxs3/proxs3d.json`. The daemon must be restarted (not just reloaded) for proxy changes to take effect:

```bash
systemctl restart proxs3d
```

## Building From Source

### Binary only

```bash
# Requires Go 1.24+
make build
sudo make install
```

### Debian package

```bash
sudo apt install debhelper golang-go build-essential
make deb
# The .deb is created in the parent directory
dpkg -i ../proxs3_*.deb
```

## Supported Content Types

| Content Type | Proxmox Value | S3 Prefix | Description |
|---|---|---|---|
| ISO images | `iso` | `template/iso/` | Installation media |
| Container templates | `vztmpl` | `template/cache/` | LXC container templates |
| Snippets | `snippets` | `snippets/` | Cloud-init configs, hookscripts |
| Backups | `backup` | `dump/` | VM/CT backup files |
| Import (disk images) | `import` | `images/` | Golden images for VM templates |

Note: ProxS3 does **not** support running VM disk images (`images`) or container rootdirs (`rootdir`) directly from S3. Live VM disks require block-level random access which S3 cannot provide. Use the `import` content type to store golden images that can be copied to local storage to create templates.

## Use Cases

### Shared ISO Library

Store installation media in S3 and make it available across all nodes in your cluster. Upload once, use everywhere — no need to copy ISOs between nodes or maintain a shared NFS mount just for a few files.

```bash
# Upload ISOs to your bucket
aws s3 cp debian-12.7-amd64-netinst.iso s3://my-bucket/template/iso/
aws s3 cp ubuntu-24.04-live-server-amd64.iso s3://my-bucket/template/iso/

# They appear in the Proxmox UI on every node immediately
```

When a node needs an ISO (e.g., to boot a VM installer), ProxS3 downloads it to the local cache on first use. Subsequent uses on the same node are served from cache with an ETag check to ensure freshness. Update an ISO in S3 and every node picks up the new version automatically.

### Golden Images for VM Templates

Store base VM disk images in S3 and import them on any node to create templates. This is ideal for maintaining a library of pre-built images (e.g., a hardened Debian base, a pre-configured application stack) that can be deployed across clusters.

```bash
# Upload golden images to the images/ prefix
aws s3 cp base-debian12-disk-0.raw s3://my-bucket/images/
aws s3 cp base-ubuntu2404-disk-0.qcow2 s3://my-bucket/images/
```

Enable the `import` content type on your S3 storage, then use Proxmox's import functionality to copy disk images to local storage and convert them into templates. The originals stay in S3 as your single source of truth.

### Shared Container Templates

Maintain a central library of LXC container templates across your cluster. Particularly useful for custom templates that aren't available from the standard Proxmox repositories.

```bash
# Upload custom container templates
aws s3 cp my-custom-debian-12_1.0_amd64.tar.zst s3://my-bucket/template/cache/
```

Templates appear in the Proxmox UI under the S3 storage. When you create a container, ProxS3 downloads the template to the local cache. Like ISOs, templates are validated against S3 on each access — update a template in the bucket and nodes will pick up the change.

### Cloud-Init Snippets

Store cloud-init user-data, network-config, and vendor-data files in S3 for use across the cluster. Keep your infrastructure-as-code configs in one place.

```bash
aws s3 cp cloud-init-user.yaml s3://my-bucket/snippets/
aws s3 cp network-config.yaml s3://my-bucket/snippets/
```

### Offsite Backups

Use S3 as a backup target for vzdump. Backups are uploaded directly to S3 and can be restored on any node. Combined with S3 lifecycle policies, this gives you cost-effective long-term backup retention without managing local disk space.

**Note:** Backup to S3 requires sufficient local cache space to stage the backup file before upload, and restore requires downloading the full backup before extraction.

## License

MIT License. See [LICENSE](LICENSE) for details.
