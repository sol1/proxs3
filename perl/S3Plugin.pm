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
            push @volumes, {
                volid   => $vol->{volume},
                format  => $vol->{format},
                size    => $vol->{size},
                content => $ct,
            };
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

sub clone_image {
    die "clone not supported on S3 storage — use --target to clone to local storage\n";
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
        for (my $i = 0; ; $i++) {
            $name = "vm-$vmid-disk-$i.$fmt";
            last if ! -e "$imagedir/$name";
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
