# ProxS3

Native S3 storage plugin for Proxmox VE. Use any S3-compatible object store as a Proxmox storage backend for ISO images, container templates, VM disk templates, snippets, and backups.

## Why Not s3fs / rclone mount?

FUSE-based S3 mounts (s3fs, rclone, goofys) present S3 as a local filesystem. This causes problems in a PVE cluster:

- **Network outages block the cluster.** When the proxy or internet goes down, the FUSE mount hangs or disappears. pvestatd tries to stat the mountpoint, blocks, and the polling loop backs up. This affects all storages on the node, not just the S3 one.
- **No offline status.** A hung FUSE mount can't report itself as offline. It blocks until the kernel times out. PVE cannot distinguish "unreachable" from "slow".
- **PVE doesn't know it's remote.** PVE treats the mount as a local directory. There is no health check, no online/offline state, and no way to tell a missing file from a network error.
- **Cache coherency.** FUSE caches are opaque to PVE. Stale reads and partial data can occur under load or network instability.

ProxS3 is a native Proxmox storage plugin. S3 operations run in a separate Go daemon, not in PVE's polling path. The local cache is a real directory on disk that always exists. When S3 is unreachable, PVE gets `online: false` from cached state within the normal polling interval. Other storages are unaffected.

## How It Works

ProxS3 has two components:

- **proxs3d** - Go daemon that handles S3 operations, maintains a local file cache, and monitors endpoint connectivity. Listens on a Unix socket.
- **S3Plugin.pm** - Perl storage plugin that registers the `s3` storage type in Proxmox. Forwards all storage operations to the daemon via the Unix socket.

```
Proxmox UI / pvesm CLI
       |
       v
  S3Plugin.pm  (PVE::Storage::Custom)
       | Unix socket (local only)
       v
    proxs3d    (Go daemon)
       | HTTPS              | local disk
       v                    v
   S3 bucket            file cache
  (source of truth)   (real directory, always exists)
```

The daemon auto-discovers S3 storages from `/etc/pve/storage.cfg`. When you add, update, or remove an S3 storage via the Proxmox UI or `pvesm`, the plugin signals the daemon to reload automatically.

### Network Failure Behaviour

When the S3 endpoint becomes unreachable (proxy down, internet outage, provider issue):

| PVE Operation | Behaviour | Effect |
|---|---|---|
| Status polling (pvestatd) | Returns `online: false` from cached state | Storage shown as unavailable. No network call, no blocking. |
| List volumes | 10s timeout, returns empty list | UI shows no contents. Does not hang. |
| Access a cached file | Serves local cached copy, skips S3 validation | Running VMs continue to work with cached ISOs/templates. |
| Access an uncached file | Returns error | File is not available without S3 connectivity. |
| Upload | File written to local cache, S3 upload fails (logged) | File preserved locally, not yet synced to S3. |

When connectivity returns, the health check detects it within 30 seconds and the storage status returns to online.

## Requirements

- Proxmox VE 9.x (Debian Trixie)
- An S3-compatible object store (AWS S3, MinIO, Ceph RGW, Cloudflare R2, Wasabi, etc.)
- Network access from each Proxmox node to the S3 endpoint (direct or via HTTP proxy)

## Quick Start

This gets you from zero to a working S3-backed ISO/template store in under 5 minutes.

### Step 1: Install the package

On **each Proxmox node** in your cluster:

```bash
# Download the latest release
wget https://github.com/sol1/proxs3/releases/latest/download/proxs3_amd64.deb

# Install
dpkg -i proxs3_amd64.deb
```

This installs:
- `/usr/bin/proxs3d` - the Go daemon
- `/usr/share/perl5/PVE/Storage/Custom/S3Plugin.pm` - the Proxmox plugin
- `/usr/share/pve-manager/js/s3storage.js` - web UI panel for the S3 storage type
- `/lib/systemd/system/proxs3d.service` - systemd unit
- `/etc/proxs3/proxs3d.json` - daemon config (only created if not already present)

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

**If you set `cache_dir` outside `/var/cache/`**, you need a systemd override so the daemon has write access (it runs with `ProtectSystem=strict`):

```bash
systemctl edit proxs3d
```

Add:

```ini
[Service]
ReadWritePaths=/mnt/proxs3-cache /run
```

Then restart: `systemctl restart proxs3d`

### Step 3: Prepare your S3 bucket

Create a bucket in your S3-compatible store and set up the expected directory structure. ProxS3 uses these key prefixes:

| Content Type | S3 Prefix | Example Key |
|---|---|---|
| ISO images | `template/iso/` | `template/iso/debian-12.7-amd64-netinst.iso` |
| Container templates | `template/cache/` | `template/cache/debian-12-standard_12.2-1_amd64.tar.zst` |
| Snippets | `snippets/` | `snippets/cloud-init-user.yaml` |
| Backups | `dump/` | `dump/vzdump-qemu-100-2024_01_01.vma.zst` |
| Import disk images | `import/` | `import/base-debian12-disk-0.raw` |
| VM disk images | `images/` | `images/9001/base-9001-disk-0.raw` |

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
# AWS S3 example - endpoint is just the hostname, no https://
pvesm add s3 my-s3-store \
    --endpoint s3.us-east-1.amazonaws.com \
    --bucket my-proxmox-bucket \
    --region us-east-1 \
    --access-key AKIAIOSFODNN7EXAMPLE \
    --secret-key wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY \
    --content iso,vztmpl,snippets,images \
    --use-ssl 1

# DigitalOcean Spaces example
pvesm add s3 my-do-store \
    --endpoint syd1.digitaloceanspaces.com \
    --bucket my-space-name \
    --region syd1 \
    --access-key DO00EXAMPLE \
    --secret-key secretkeyhere \
    --content iso,vztmpl,snippets \
    --use-ssl 1

# MinIO example - path-style 1 required
pvesm add s3 my-minio-store \
    --endpoint minio.local:9000 \
    --bucket my-bucket \
    --content iso,vztmpl,snippets \
    --path-style 1
```

**What happens behind the scenes:**

1. The plugin writes your credentials to `/etc/pve/priv/proxs3/my-s3-store.json` (root-only, 0600). This file is cluster-shared via pmxcfs.
2. The storage configuration (endpoint, bucket, region, etc.) is written to `/etc/pve/storage.cfg`, but **credentials are not stored there**.
3. The plugin signals proxs3d to reload its configuration.
4. The daemon re-reads `storage.cfg`, discovers the new S3 storage, loads its credentials, and starts health-checking the endpoint.

### Step 6: Verify

```bash
# Check storage status
pvesm status

# List contents
pvesm list my-s3-store

# Or check the web UI - your S3 storage should appear with a green status indicator
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
| `endpoint` | Yes | S3 endpoint hostname (see **Endpoint and URL Style** below) |
| `bucket` | Yes | S3 bucket name |
| `region` | No | S3 region (defaults to `us-east-1`) |
| `access-key` | No | S3 access key ID (omit for public buckets) |
| `secret-key` | No | S3 secret access key (omit for public buckets) |
| `content` | No | Comma-separated content types: `iso`, `vztmpl`, `snippets`, `backup`, `import`, `images` |
| `use-ssl` | No | Use HTTPS (`1`) or HTTP (`0`). Defaults to on. |
| `path-style` | No | URL style (see **Endpoint and URL Style** below) |
| `cache-max-age` | No | Maximum age of cached files in days. `0` (default) keeps files forever. See **Cache Age Eviction** below. |
| `nodes` | No | Restrict storage to specific cluster nodes |

### Endpoint and URL Style

The **endpoint** field must be the **base hostname only**: no `https://` prefix, no bucket name, no trailing slash.

S3 has two URL styles for addressing buckets:

| Style | URL Format | `path-style` | Example |
|---|---|---|---|
| **Virtual-hosted** (default) | `https://BUCKET.ENDPOINT/key` | `0` | `https://my-bucket.s3.amazonaws.com/template/iso/debian.iso` |
| **Path** | `https://ENDPOINT/BUCKET/key` | `1` | `https://minio.local:9000/my-bucket/template/iso/debian.iso` |

**Common mistake:** If your provider gives you a URL like `https://my-bucket.syd1.digitaloceanspaces.com`, the endpoint is just `syd1.digitaloceanspaces.com`. The bucket name is a separate field. Don't include it in the endpoint or it will be doubled.

**How to choose:**
- **AWS S3, DigitalOcean Spaces, Wasabi, Cloudflare R2, Backblaze B2** → Virtual-hosted (`path-style 0`, the default)
- **MinIO, Ceph RGW, most self-hosted S3** → Path style (`path-style 1`)

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

| Provider | `endpoint` | `region` | `use-ssl` | `path-style` |
|---|---|---|---|---|
| AWS S3 | `s3.us-east-1.amazonaws.com` | `us-east-1` | `1` | `0` |
| AWS S3 (other region) | `s3.ap-southeast-2.amazonaws.com` | `ap-southeast-2` | `1` | `0` |
| DigitalOcean Spaces | `syd1.digitaloceanspaces.com` | `syd1` | `1` | `0` |
| Wasabi | `s3.wasabisys.com` | `us-east-1` | `1` | `0` |
| Cloudflare R2 | `<account-id>.r2.cloudflarestorage.com` | `auto` | `1` | `0` |
| Backblaze B2 | `s3.us-west-004.backblazeb2.com` | `us-west-004` | `1` | `0` |
| MinIO | `minio.local:9000` | | depends | `1` |
| Ceph RGW | `rgw.local:7480` | | depends | `1` |

**Note:** The endpoint is always just the hostname (and port if non-standard). Never include `https://`, the bucket name, or a trailing slash.

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

### Cache Age Eviction

The `cache-max-age` storage property controls how long files stay in the local cache. This is a **per-storage** setting, configured in Proxmox (not in the daemon config), so different storages can have different policies.

**Why per-storage?** A single daemon often serves multiple storages - for example, one for ISOs and one for backups. You probably want ISOs cached indefinitely (they're accessed repeatedly) but backup files cleaned up after a few days (they're written once, synced to S3, and rarely accessed locally again).

```bash
# Keep backup cache for 7 days
pvesm set my-backup-store --cache-max-age 7

# Keep ISOs forever (default, no need to set)
pvesm set my-iso-store --cache-max-age 0
```

The daemon checks file ages hourly. Files older than `cache-max-age` days (by modification time) are removed from the local cache. The files remain in S3 and will be re-downloaded if needed.

This is separate from `cache_max_mb` (the daemon-wide size limit) and from `prune-backups` (which controls backup retention in S3 itself, not the local cache).

## Multi-Node Clusters and Shared Storage

ProxS3 registers as **shared storage** in PVE, meaning the cluster treats it like NFS or Ceph — all nodes see the same data. This is correct because S3 is inherently shared: every node accesses the same bucket.

- **Add the storage once.** `storage.cfg` is shared across all nodes via pmxcfs. After `pvesm add`, every node sees the storage.
- **Credentials are cluster-shared.** Stored in `/etc/pve/priv/proxs3/`, distributed by pmxcfs. Root-only permissions (0600).
- **Install the .deb on each node.** The daemon and plugin must be present on every node that needs access.
- **Cache is per-node.** Each node maintains its own local cache. Nodes pull from S3 independently and validate against S3 metadata.
- **Daemon config is per-node.** `/etc/proxs3/proxs3d.json` is local to each node, so you can set different cache paths and sizes per node.

### How shared storage works with a local cache

Traditional shared storage (NFS, Ceph) provides a single filesystem visible to all nodes simultaneously. ProxS3 is different — S3 is the shared source of truth, but each node has an independent local cache.

This works because:

1. **Volume listing queries S3 directly**, not the cache. When PVE lists available ISOs or templates, every node sees the same results from the bucket.
2. **`activate_volume` downloads on demand.** When a node needs a file, it downloads from S3 to its local cache. The cache is validated against S3 metadata (ETag) on each access, so stale copies are automatically refreshed.
3. **Uploads propagate via S3.** When a file is uploaded on one node, the watcher uploads it to S3. Other nodes see it on their next volume listing. There is a brief window (seconds) between upload and S3 visibility.

For a read-heavy, write-rarely workload (ISOs, templates, golden disk images), this model works well. Files are written once, read many times, and the cache ensures fast local access after the first download.

### What shared storage enables

- **Backups visible cluster-wide.** A backup created on node A appears in PVE's backup list on all nodes.
- **Live migration.** PVE skips disk transfer for shared storage during migration (though VMs should not have S3-backed live disks — see limitations below).
- **HA failover.** PVE knows the storage is accessible from any node.
- **Template cloning from any node.** The same S3-backed template is available on every node in every cluster that has the storage configured.

### Limitations of the shared model

- **No live VM disks.** S3 cannot provide block-level random access. VMs must run from local storage. The `images` content type is for template disks that are cloned to local storage before use.
- **No linked clones.** Linked clones create a qcow2 overlay referencing the base image in the cache. If the cache evicts the base image, the overlay breaks. Always use full clones (`--full`) when cloning from S3 templates.
- **Cache eviction affects availability.** If a cached file is evicted, the next access triggers a re-download from S3. For large files over slow links, this adds latency. Size your cache (`cache_max_mb`) and age policy (`cache-max-age`) to keep frequently-used files cached.
- **Upload visibility delay.** After uploading a file, there is a brief delay (typically seconds) before it appears in S3 and becomes visible to other nodes.

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
| Import (disk images) | `import` | `import/` | Importable disk images via `import-from=` |
| VM disk images | `images` | `images/` | VM disk templates for cloning (see below) |

**Note:** ProxS3 does not support running VMs with disks on S3. Live VM disks require block-level random access which S3 cannot provide. Container rootdirs (`rootdir`) are also not supported. The `images` content type is for **template disks that are cloned to local storage** before use — VMs run from the local clone, not from S3 directly.

## Use Cases

### Shared ISO Library

Store installation media in S3 and make it available across all nodes in your cluster. Upload once, available on every node. No need to copy ISOs between nodes or maintain a shared NFS mount.

```bash
# Upload ISOs to your bucket
aws s3 cp debian-12.7-amd64-netinst.iso s3://my-bucket/template/iso/
aws s3 cp ubuntu-24.04-live-server-amd64.iso s3://my-bucket/template/iso/

# They appear in the Proxmox UI on every node immediately
```

When a node needs an ISO (e.g., to boot a VM installer), ProxS3 downloads it to the local cache on first use. Subsequent uses on the same node are served from cache with an ETag check to ensure freshness. Update an ISO in S3 and every node picks up the new version automatically.

### VM Disk Templates (images)

Store VM template disks in S3 and clone them to local storage on any node. This is ideal for maintaining a central library of golden images (e.g., a hardened Debian base, a pre-configured application stack) shared across multiple PVE clusters.

```bash
# Upload template disks — preserving the vmid/diskname structure PVE expects
aws s3 cp base-9001-disk-0.raw s3://my-bucket/images/9001/base-9001-disk-0.raw
```

Create a PVE template VM with its disks on S3 storage:

```bash
pvesm add s3 my-s3-store \
    --endpoint s3.ap-southeast-2.amazonaws.com \
    --bucket my-proxmox-images \
    --region ap-southeast-2 \
    --content images,iso,vztmpl \
    --use-ssl 1
```

Then clone the template to local storage:

```bash
# Full clone — copies the disk from S3 cache to local storage
qm clone 9001 200 --name my-new-vm --full --target local-lvm
```

The first clone on each node downloads the disk from S3 to the local cache. Subsequent clones on the same node are served from cache (validated against S3 via ETag). Update the image in S3 and new clones automatically get the latest version.

**Limitations:**
- VMs cannot run with disks on S3 — always clone to local storage first
- Linked clones are not supported (they require random access to the base image)
- Use `--target` to specify local storage when cloning, or PVE will try to clone within S3

### Golden Images for Import (import)

For a simpler workflow without PVE template VMs, use the `import` content type with PVE's `import-from` syntax:

```bash
# Upload disk images to the import/ prefix
aws s3 cp base-debian12-disk-0.raw s3://my-bucket/import/
```

Then import directly when creating a VM:

```bash
qm create 200 --name my-new-vm \
    --scsi0 local-lvm:0,import-from=my-s3-store:import/base-debian12-disk-0.raw
```

This pulls the disk from S3 and writes it directly to local storage. Use `images` for the template/clone workflow, or `import` for one-shot imports — both work.

### Shared Container Templates

Maintain a central library of LXC container templates across your cluster. Particularly useful for custom templates that aren't available from the standard Proxmox repositories.

```bash
# Upload custom container templates
aws s3 cp my-custom-debian-12_1.0_amd64.tar.zst s3://my-bucket/template/cache/
```

Templates appear in the Proxmox UI under the S3 storage. When you create a container, ProxS3 downloads the template to the local cache. Like ISOs, templates are validated against S3 on each access. Update a template in the bucket and nodes pick up the change automatically.

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
