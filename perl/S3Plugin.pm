package PVE::Storage::Custom::S3Plugin;

use strict;
use warnings;

use base qw(PVE::Storage::Plugin);

use JSON;
use File::Basename;
use File::Path qw(make_path);
use IO::Socket::UNIX;
use POSIX qw(SIGTERM SIGHUP);

my $SOCKET_PATH = '/run/proxs3d.sock';
my $CRED_DIR = '/etc/pve/priv/proxs3';
my $DEFAULT_CACHE_DIR = '/var/cache/proxs3';

# Learned from daemon at activation time — never queried in check_config
my $_cache_dir;

# PVE storage plugin API version (must match or be within APIAGE of PVE::Storage::APIVER)
use constant APIVERSION => 13;

sub api {
    return APIVERSION;
}

# Register as storage type 's3'
sub type {
    return 's3';
}

sub plugindata {
    return {
        content => [
            { images => 1, rootdir => 0, vztmpl => 1, iso => 1, backup => 1, snippets => 1, none => 0, import => 1 },
            { iso => 1, vztmpl => 1, snippets => 1 },
        ],
        format => [ { raw => 1 } , 'raw' ],
        'sensitive-properties' => {
            'access-key' => 1,
            'secret-key' => 1,
        },
    };
}

sub properties {
    return {
        endpoint => {
            description => "S3 endpoint hostname (e.g., s3.amazonaws.com or minio.local:9000)",
            type        => 'string',
        },
        bucket => {
            description => "S3 bucket name",
            type        => 'string',
        },
        region => {
            description => "S3 region",
            type        => 'string',
            optional    => 1,
        },
        'access-key' => {
            description => "S3 access key ID",
            type        => 'string',
        },
        'secret-key' => {
            description => "S3 secret access key",
            type        => 'string',
        },
        'use-ssl' => {
            description => "Use HTTPS to connect to S3",
            type        => 'boolean',
            optional    => 1,
        },
        'path-style' => {
            description => "Use path-style S3 access (required for MinIO and most S3-compatible stores)",
            type        => 'boolean',
            optional    => 1,
        },
        'cache-max-age' => {
            description => "Maximum age of cached files in days (0 = keep forever, default). "
                         . "Useful for backup storages where local cache should be cleaned up after sync.",
            type        => 'integer',
            minimum     => 0,
            optional    => 1,
        },
    };
}

sub options {
    return {
        endpoint     => { optional => 0 },
        bucket       => { optional => 0 },
        region       => { optional => 1 },
        'access-key' => { optional => 1 },
        'secret-key' => { optional => 1 },
        'use-ssl'    => { optional => 1 },
        'path-style' => { optional => 1 },
        content      => { optional => 1 },
        nodes        => { optional => 1 },
        disable      => { optional => 1 },
        'max-protected-backups' => { optional => 1 },
        'prune-backups'  => { optional => 1 },
        'cache-max-age'  => { optional => 1 },
    };
}

# --- Credential management (stored in /etc/pve/priv/proxs3/) ---

sub _cred_path {
    my ($storeid) = @_;
    return "$CRED_DIR/$storeid.json";
}

sub _write_credentials {
    my ($storeid, $access_key, $secret_key) = @_;

    mkdir $CRED_DIR if ! -d $CRED_DIR;

    my $cred = encode_json({
        access_key => $access_key,
        secret_key => $secret_key,
    });

    my $path = _cred_path($storeid);
    open(my $fh, '>', $path) or die "Cannot write credentials to $path: $!\n";
    chmod 0600, $fh;
    print $fh $cred;
    close $fh;
}

sub _read_credentials {
    my ($storeid) = @_;

    my $path = _cred_path($storeid);
    open(my $fh, '<', $path) or die "Cannot read credentials from $path: $!\n";
    local $/;
    my $data = <$fh>;
    close $fh;

    return decode_json($data);
}

# Signal the daemon to reload its config (re-reads storage.cfg)
sub _reload_daemon {
    my $pidfile = '/run/proxs3d.pid';
    if (open(my $fh, '<', $pidfile)) {
        my $pid = <$fh>;
        close $fh;
        chomp $pid;
        # Untaint the PID (PVE runs with -T)
        if ($pid && $pid =~ /^(\d+)$/) {
            kill SIGHUP, $1;
            return;
        }
    }
    # Fallback: try systemctl
    system('systemctl', 'reload', 'proxs3d') == 0 or
        warn "Could not reload proxs3d daemon\n";
}

# --- Helper: talk to proxs3d via Unix socket ---

sub _daemon_request {
    my ($path, $params) = @_;

    my $query = join('&', map { "$_=" . _uri_encode($params->{$_}) } keys %$params);

    my $sock = IO::Socket::UNIX->new(
        Peer    => $SOCKET_PATH,
        Type    => SOCK_STREAM,
        Timeout => 10,
    ) or die "Cannot connect to proxs3d at $SOCKET_PATH: $!\n";

    my $req = "GET ${path}?${query} HTTP/1.0\r\nHost: localhost\r\n\r\n";
    $sock->print($req);

    my $response = '';
    while (my $line = <$sock>) {
        $response .= $line;
    }
    $sock->close();

    # Parse HTTP response: skip headers, get body
    my ($headers, $body) = split(/\r?\n\r?\n/, $response, 2);

    # Check for HTTP errors
    if ($headers && $headers =~ m{^HTTP/\S+\s+(\d+)}) {
        my $code = $1;
        if ($code >= 400) {
            die "proxs3d error $code: $body\n";
        }
    }

    return decode_json($body // '{}');
}

sub _uri_encode {
    my ($str) = @_;
    $str =~ s/([^A-Za-z0-9\-_.~])/sprintf("%%%02X", ord($1))/ge;
    return $str;
}

# --- Plugin Lifecycle Hooks ---

sub on_add_hook {
    my ($class, $storeid, $scfg, %param) = @_;

    my $access_key = $param{'access-key'};
    my $secret_key = $param{'secret-key'};

    if ($access_key && $secret_key) {
        _write_credentials($storeid, $access_key, $secret_key);
    }
    # Don't reload here — storage.cfg hasn't been written yet.
    # The reload happens in activate_storage which runs after config is saved.
    return;
}

sub on_update_hook {
    my ($class, $storeid, $scfg, %param) = @_;

    my $access_key = $param{'access-key'};
    my $secret_key = $param{'secret-key'};

    if ($access_key && $secret_key) {
        _write_credentials($storeid, $access_key, $secret_key);
    }
    # Reload here — on update, storage.cfg is already written
    _reload_daemon();
    return;
}

sub on_delete_hook {
    my ($class, $storeid, $scfg) = @_;

    my $path = _cred_path($storeid);
    unlink $path if -f $path;
    _reload_daemon();
    return;
}

# --- Plugin Interface Methods ---

sub check_config {
    my ($self, $sectionId, $param, $create, $skipSchemaCheck) = @_;
    my $opts = $self->SUPER::check_config($sectionId, $param, $create, $skipSchemaCheck);

    # Set path for PVE's upload flow — must match daemon's cache_dir for large uploads
    # Untaint: $sectionId and $_cache_dir come from PVE/daemon (tainted under -T),
    # but PVE's base class uses $scfg->{path} in filesystem_path() → exec (qemu-img).
    my $cache_base = $_cache_dir // $DEFAULT_CACHE_DIR;
    ($cache_base) = $cache_base =~ /\A([a-zA-Z0-9._\/-]+)\z/ or die "Invalid cache dir: $cache_base\n";
    (my $safe_sid) = $sectionId =~ /\A([a-zA-Z0-9._-]+)\z/ or die "Invalid storage id: $sectionId\n";
    $opts->{path} = "$cache_base/$safe_sid";

    # S3 is inherently shared — all nodes see the same bucket
    $opts->{shared} = 1;

    # Set target so PVE shows the S3 endpoint in the Path/Target column
    my $endpoint = $param->{endpoint} // $opts->{endpoint} // '';
    my $bucket = $param->{bucket} // $opts->{bucket} // '';
    $opts->{target} = "${bucket}\@${endpoint}" if $endpoint;

    return $opts;
}

sub activate_storage {
    my ($class, $storeid, $scfg, $cache) = @_;

    # Check if daemon already knows this storage — if not, reload
    my $needs_reload = 0;
    eval {
        _daemon_request('/v1/status', { storage => $storeid });
    };
    if ($@) {
        $needs_reload = 1;
    }

    if ($needs_reload) {
        sleep 1; # pmxcfs sync delay
        _reload_daemon();

        # Re-check after reload
        eval {
            _daemon_request('/v1/status', { storage => $storeid });
        };
        if ($@) {
            warn "proxs3: storage '$storeid' activation warning: $@";
        }
    }

    # Learn cache_dir from daemon (safe here — activate_storage is not in the polling loop)
    eval {
        my $conf = _daemon_request('/v1/config', {});
        $_cache_dir = $conf->{cache_dir} if $conf && $conf->{cache_dir};
    };

    # Ensure the cache path exists for PVE's upload flow
    # Untaint storeid and cache_base — PVE validates these before they reach us,
    # but File::Path::make_path enforces taint checks unlike basic mkdir/open.
    my $cache_base = $_cache_dir // $DEFAULT_CACHE_DIR;
    ($cache_base) = $cache_base =~ /\A([a-zA-Z0-9._\/-]+)\z/ or die "Invalid cache dir: $cache_base\n";
    (my $safe_sid) = $storeid =~ /\A([a-zA-Z0-9._-]+)\z/ or die "Invalid storage id: $storeid\n";
    my $path = "$cache_base/$safe_sid";
    for my $sub (qw(template/iso template/cache snippets dump import images)) {
        my $dir = "$path/$sub";
        File::Path::make_path($dir) if ! -d $dir;
    }

    return 1;
}

sub deactivate_storage {
    my ($class, $storeid, $scfg, $cache) = @_;
    return 1;
}

sub check_connection {
    my ($class, $storeid, $scfg) = @_;
    return -S $SOCKET_PATH ? 1 : 0;
}

sub status {
    my ($class, $storeid, $scfg, $cache) = @_;

    my $res = eval { _daemon_request('/v1/status', { storage => $storeid }) };
    if ($@) {
        return (0, 0, 0, 0);
    }

    my $total = $res->{total} // 0;
    my $avail = $res->{available} // 0;
    my $used  = $res->{used} // 0;
    my $active = $res->{online} ? 1 : 0;

    return ($total, $avail, $used, $active);
}

sub list_volumes {
    my ($class, $storeid, $scfg, $vmid, $content_types) = @_;

    my @volumes;
    for my $ct (@$content_types) {
        my $list = eval {
            _daemon_request('/v1/list', { storage => $storeid, content => $ct });
        };
        next if $@ || !$list || ref($list) ne 'ARRAY';

        for my $vol (@$list) {
            my $info = {
                volid   => $vol->{volume},
                format  => $vol->{format},
                size    => $vol->{size},
                content => $ct,
            };
            # images volnames are "vmid/diskname" — PVE UI needs the vmid field
            if ($ct eq 'images' && $vol->{volume} =~ m/^[^:]+:(\d+)\//) {
                $info->{vmid} = $1;
            }
            push @volumes, $info;
        }
    }
    return \@volumes;
}

sub path {
    my ($class, $scfg, $volname, $storeid, $snapname) = @_;

    my ($content, $filename) = _parse_volname($volname);
    my $prefix = _content_to_prefix($content);
    my $key = "${prefix}${filename}";

    my $res = eval {
        _daemon_request('/v1/path', { storage => $storeid, key => $key });
    };
    if ($@ || !$res->{path}) {
        die "Failed to resolve path for $volname: $@\n";
    }

    # Untaint path from daemon — data over Unix socket is tainted under -T
    my ($safe_path) = $res->{path} =~ /\A([a-zA-Z0-9._\/-]+)\z/
        or die "Invalid path from daemon: $res->{path}\n";

    return ($safe_path, undef, $content);
}

# Upload: called by Proxmox when user uploads an ISO/template via UI or API
sub upload {
    my ($class, $storeid, $scfg, $volname, $tmpfile) = @_;

    my ($content, $filename) = _parse_volname($volname);
    my $prefix = _content_to_prefix($content);
    my $key = "${prefix}${filename}";

    my $res = eval {
        _daemon_request('/v1/upload', {
            storage => $storeid,
            key     => $key,
            path    => $tmpfile,
        });
    };
    if ($@) {
        die "Failed to upload $volname: $@\n";
    }

    return;
}

sub activate_volume {
    my ($class, $storeid, $scfg, $volname, $snapname, $cache, $hints) = @_;

    # Download from S3 to local cache on first access
    my ($content, $filename) = _parse_volname($volname);
    my $prefix = _content_to_prefix($content);
    my $key = "${prefix}${filename}";

    my $res = eval {
        _daemon_request('/v1/download', { storage => $storeid, key => $key });
    };
    if ($@ || !$res->{path}) {
        die "volume '$storeid:$volname' does not exist\n";
    }

    return 1;
}

sub volume_has_feature {
    my ($class, $scfg, $feature, $storeid, $volname, $snapname, $running, $opts) = @_;

    # S3 supports these features for images content
    my $dominated = {
        clone    => 1,  # same-storage full copy via S3 CopyObject
        copy     => 1,  # cross-storage copy (activate + path + qemu-img)
        template => 1,  # convert to base image (rename in S3)
        rename   => 1,  # reassign disk ownership (rename in S3)
        sparseinit => 1,
    };

    # No snapshot support — S3 has no snapshot semantics
    return 0 if ($feature eq 'snapshot');

    return $dominated->{$feature} ? 1 : 0;
}

sub create_base {
    my ($class, $storeid, $scfg, $volname) = @_;

    my ($content, $filename) = _parse_volname($volname);
    die "create_base only works for images content\n" if $content ne 'images';

    # Parse vmid and disk name: "9001/vm-9001-disk-0.raw" → base-9001-disk-0.raw
    my ($vmid, $diskname) = $filename =~ m|^(\d+)/(.+)$|
        or die "Invalid images volname: $filename\n";
    my $basename = $diskname;
    $basename =~ s/^vm-/base-/;

    my $src_key = "images/$vmid/$diskname";
    my $dst_key = "images/$vmid/$basename";

    # Rename in S3 (copy + delete)
    eval {
        _daemon_request('/v1/rename', {
            storage => $storeid,
            src_key => $src_key,
            dst_key => $dst_key,
        });
    };
    die "create_base: S3 rename failed: $@\n" if $@;

    return "$vmid/$basename";
}

sub clone_image {
    my ($class, $scfg, $storeid, $volname, $vmid, $snap) = @_;

    die "snapshots not supported on S3 storage\n" if $snap;

    my ($content, $filename) = _parse_volname($volname);
    die "clone only works for images content\n" if $content ne 'images';

    my $src_key = "images/$filename";

    # Allocate a new disk name for the clone
    my $name = $class->find_free_diskname($storeid, $scfg, $vmid, 'raw');
    my $dst_key = "images/$vmid/$name";

    # Copy in S3 (keep source — this is a clone, not a move)
    eval {
        _daemon_request('/v1/copy', {
            storage => $storeid,
            src_key => $src_key,
            dst_key => $dst_key,
        });
    };
    die "clone_image: S3 copy failed: $@\n" if $@;

    return "$vmid/$name";
}

sub rename_volume {
    my ($class, $scfg, $storeid, $source_volname, $target_vmid, $target_volname) = @_;

    my ($src_content, $src_filename) = _parse_volname($source_volname);
    die "rename only works for images content\n" if $src_content ne 'images';

    my $src_key = "images/$src_filename";

    # Build target volname if not provided
    if (!$target_volname) {
        my ($src_vmid, $diskname) = $src_filename =~ m|^(\d+)/(.+)$|
            or die "Invalid images volname: $src_filename\n";
        $diskname =~ s/^(vm|base)-\d+/vm-$target_vmid/;
        $target_volname = "$target_vmid/$diskname";
    }

    my ($tgt_content, $tgt_filename) = _parse_volname($target_volname);
    my $dst_key = "images/$tgt_filename";

    eval {
        _daemon_request('/v1/rename', {
            storage => $storeid,
            src_key => $src_key,
            dst_key => $dst_key,
        });
    };
    die "rename_volume: S3 rename failed: $@\n" if $@;

    return "$target_volname";
}

sub alloc_image {
    my ($class, $storeid, $scfg, $vmid, $fmt, $name, $size) = @_;

    $fmt //= 'raw';

    # Build and untaint the images directory path
    my $cache_base = $_cache_dir // $DEFAULT_CACHE_DIR;
    ($cache_base) = $cache_base =~ /\A([a-zA-Z0-9._\/-]+)\z/ or die "Invalid cache dir: $cache_base\n";
    (my $safe_sid) = $storeid =~ /\A([a-zA-Z0-9._-]+)\z/ or die "Invalid storage id: $storeid\n";
    ($vmid) = $vmid =~ /\A(\d+)\z/ or die "Invalid vmid: $vmid\n";
    my $imagedir = "$cache_base/$safe_sid/images/$vmid";
    File::Path::make_path($imagedir);

    # Untaint format and size for qemu-img exec
    ($fmt) = $fmt =~ /\A(raw|qcow2|vmdk)\z/ or die "Invalid format: $fmt\n";
    ($size) = $size =~ /\A(\d+)\z/ or die "Invalid size: $size\n";

    if (!$name) {
        # Use find_free_diskname which queries S3 via list_images
        $name = $class->find_free_diskname($storeid, $scfg, $vmid, $fmt);

        # Also check local cache — previous allocations may not have been
        # uploaded to S3 yet, so find_free_diskname won't see them.
        while (-e "$imagedir/$name") {
            if ($name =~ /^(vm-\d+-disk-)(\d+)(.*)$/) {
                $name = $1 . ($2 + 1) . $3;
            } else {
                die "alloc_image: cannot find free disk name for VM $vmid\n";
            }
        }
    }

    # Create the disk image in the cache — the watcher uploads to S3
    my $path = "$imagedir/$name";
    PVE::Tools::run_command(['/usr/bin/qemu-img', 'create', '-f', $fmt, $path, "${size}K"]);

    return "$vmid/$name";
}

sub free_image {
    my ($class, $storeid, $scfg, $volname, $isBase) = @_;

    my ($content, $filename) = _parse_volname($volname);
    my $prefix = _content_to_prefix($content);
    my $key = "${prefix}${filename}";

    eval {
        _daemon_request('/v1/delete', { storage => $storeid, key => $key });
    };
    warn "proxs3: failed to delete $volname: $@\n" if $@;
    return;
}

sub list_images {
    my ($class, $storeid, $scfg, $vmid, $vollist, $cache) = @_;

    # Query S3 instead of globbing local cache — the base class globs the
    # filesystem which only sees cached files, missing images in S3.
    my $list = eval {
        _daemon_request('/v1/list', { storage => $storeid, content => 'images' });
    };
    return [] if $@ || !$list || ref($list) ne 'ARRAY';

    my @volumes;
    for my $vol (@$list) {
        my $info = {
            volid   => $vol->{volume},
            format  => $vol->{format},
            size    => $vol->{size},
            content => 'images',
        };
        if ($vol->{volume} =~ m/^[^:]+:(\d+)\//) {
            $info->{vmid} = $1;
            next if defined($vmid) && $info->{vmid} != $vmid;
        }
        push @volumes, $info;
    }
    return \@volumes;
}

sub volume_size_info {
    my ($class, $scfg, $storeid, $volname, $timeout) = @_;

    my ($content, $filename) = _parse_volname($volname);
    my $prefix = _content_to_prefix($content);
    my $key = "${prefix}${filename}";

    # Try local cache first (qemu-img info gives accurate virtual/used size)
    my $cache_base = $_cache_dir // $DEFAULT_CACHE_DIR;
    my $cached = "$cache_base/$storeid/$key";
    if (-e $cached) {
        # Untaint for exec
        ($cached) = $cached =~ /\A([a-zA-Z0-9._\/-]+)\z/;
        if ($cached) {
            my ($size, $format, $used, $parent) = PVE::Storage::Plugin::file_size_info($cached, $timeout);
            return wantarray ? ($size, $format, $used, $parent) : $size
                if $size;
        }
    }

    # Fall back to S3 metadata (HeadObject) — only gives object size, not virtual size
    my $res = eval {
        _daemon_request('/v1/list', { storage => $storeid, content => $content });
    };
    if (!$@ && $res && ref($res) eq 'ARRAY') {
        for my $vol (@$res) {
            if ($vol->{key} eq $key) {
                my $format = $vol->{format} // 'raw';
                return wantarray ? ($vol->{size}, $format, $vol->{size}, undef) : $vol->{size};
            }
        }
    }

    return wantarray ? (0, 'raw', 0, undef) : 0;
}

sub volume_resize {
    die "volume resize is not supported on S3 storage\n";
}

sub prune_backups {
    my ($class, $scfg, $storeid, $keep, $vmid, $type, $dryrun, $logfunc) = @_;

    $logfunc //= sub { print @_ };

    # List backup volumes from S3
    my $list = eval {
        _daemon_request('/v1/list', { storage => $storeid, content => 'backup' });
    };
    return if $@ || !$list || ref($list) ne 'ARRAY';

    # Build volume list in the format PVE::Storage::prune_mark_backup_group expects
    my @backups;
    for my $vol (@$list) {
        my $volid = $vol->{volume};
        my ($sid, $volname) = $volid =~ m/^([^:]+):(.+)$/;
        next unless $volname;

        # Filter by vmid if specified
        my $archive_info = eval { PVE::Storage::archive_info($volname) };
        next if $@;
        next if defined($vmid) && $archive_info->{vmid} != $vmid;
        next if defined($type) && $archive_info->{type} ne $type;

        push @backups, {
            volid  => $volid,
            ctime  => $archive_info->{ctime},
            vmid   => $archive_info->{vmid},
            type   => $archive_info->{type},
            mark   => 'keep',
        };
    }

    PVE::Storage::prune_mark_backup_group(\@backups, $keep);

    for my $backup (@backups) {
        my $volid = $backup->{volid};
        my $mark = $backup->{mark};

        if ($mark eq 'remove') {
            # Prune from local cache only — S3 backups are preserved.
            # Use S3 lifecycle policies to manage long-term retention in the bucket.
            $logfunc->("prune cache: $volid\n");
            if (!$dryrun) {
                eval {
                    my ($sid, $volname) = $volid =~ m/^([^:]+):(.+)$/;
                    my ($content, $filename) = _parse_volname($volname);
                    my $prefix = _content_to_prefix($content);
                    my $key = "${prefix}${filename}";
                    my $cache_base = $_cache_dir // $DEFAULT_CACHE_DIR;
                    my $cached = "$cache_base/$storeid/$key";
                    unlink $cached if -e $cached;
                };
                $logfunc->("  error: $@") if $@;
            }
        } elsif ($mark eq 'keep') {
            $logfunc->("keep: $volid\n");
        } elsif ($mark eq 'protected') {
            $logfunc->("protected: $volid\n");
        } else {
            $logfunc->("unknown: $volid\n");
        }
    }

    return \@backups;
}

# --- Helpers ---

# PVE API: parse_volname returns ($vtype, $name, $vmid, $basename, $basevmid, $isBase, $format).
# We override the base class to handle our simple content/filename format and untaint the
# values — the base class parse_volname returns tainted strings extracted from $volname,
# which propagate through filesystem_path() into exec() and trigger taint-mode errors.
sub parse_volname {
    my ($class, $volname) = @_;
    my ($content, $filename) = _parse_volname($volname);
    # Untaint via regex — these values are validated by PVE before reaching us
    ($content) = $content =~ /\A([a-zA-Z0-9_-]+)\z/ or die "Invalid content type: $content\n";
    ($filename) = $filename =~ /\A([a-zA-Z0-9._\/-]+)\z/ or die "Invalid filename: $filename\n";
    my $format = $filename =~ /\.(raw|qcow2|vmdk)$/i ? lc($1) : 'raw';
    # images volnames include vmid: "9001/disk-0.raw" → vmid=9001, name=disk-0.raw
    my $vmid;
    if ($content eq 'images') {
        ($vmid, my $name) = $filename =~ m|^(\d+)/(.+)$|;
        $filename = $name if $name;
    }
    return ($content, $filename, $vmid, undef, undef, undef, $format);
}

sub _parse_volname {
    my ($volname) = @_;
    my $content = 'iso';
    my $filename = $volname;
    if ($volname =~ m|^(\d+)/(.+)$|) {
        # Numeric first component is images content: "9001/disk-0.raw"
        $content  = 'images';
        $filename = "$1/$2";  # preserve vmid in filename for S3 key
    } elsif ($volname =~ m|^([^/]+)/(.+)$|) {
        $content  = $1;
        $filename = $2;
    }
    return ($content, $filename);
}

sub _content_to_prefix {
    my ($content) = @_;
    my %map = (
        iso      => 'template/iso/',
        vztmpl   => 'template/cache/',
        snippets => 'snippets/',
        backup   => 'dump/',
        import   => 'import/',
        images   => 'images/',
    );
    return $map{$content} // "${content}/";
}

1;

__END__

=head1 NAME

PVE::Storage::Custom::S3Plugin - S3-backed storage plugin for Proxmox VE

=head1 DESCRIPTION

This plugin provides native S3 storage support for Proxmox VE, handling
ISO images, container templates, snippets, and backups.

It communicates with the proxs3d daemon over a Unix socket for all S3
operations. The daemon handles connection pooling, caching, retries,
and health monitoring.

=head1 CONFIGURATION

Add to /etc/pve/storage.cfg (via the web UI or pvesm):

    s3: my-s3-store
        endpoint s3.amazonaws.com
        bucket my-proxmox-bucket
        region us-east-1
        content iso,vztmpl,snippets
        use-ssl 1
        path-style 0

Credentials are stored separately in /etc/pve/priv/proxs3/<storeid>.json
(cluster-shared, root-only). They are written automatically when you
configure the storage via the UI or pvesm. The file format is:

    {"access_key": "AKID...", "secret_key": "SECRET..."}

=cut
