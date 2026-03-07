package PVE::Storage::Custom::S3Plugin;

use strict;
use warnings;

use base qw(PVE::Storage::Plugin);

use HTTP::Tiny;
use JSON;
use File::Basename;
use IO::Socket::UNIX;

my $SOCKET_PATH = '/run/proxs3d.sock';

# Register as storage type 's3'
sub type {
    return 's3';
}

sub plugindata {
    return {
        content => [
            { images => 0, rootdir => 0, vztmpl => 1, iso => 1, backup => 1, snippets => 1, none => 0 },
            { iso => 1, vztmpl => 1, snippets => 1 },
        ],
        format => [ { raw => 1 } , 'raw' ],
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
            description => "S3 access key ID (stored in /etc/pve/priv/proxs3/<storeid>.json, not in storage.cfg)",
            type        => 'string',
        },
        'secret-key' => {
            description => "S3 secret access key (stored in /etc/pve/priv/proxs3/<storeid>.json, not in storage.cfg)",
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
    };
}

sub options {
    return {
        endpoint     => { optional => 0 },
        bucket       => { optional => 0 },
        region       => { optional => 1 },
        'access-key' => { optional => 0 },
        'secret-key' => { optional => 0 },
        'use-ssl'    => { optional => 1 },
        'path-style' => { optional => 1 },
        content      => { optional => 1 },
        nodes        => { optional => 1 },
        disable      => { optional => 1 },
        'max-protected-backups' => { optional => 1 },
    };
}

my $CRED_DIR = '/etc/pve/priv/proxs3';

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

# --- Helper: talk to proxs3d via Unix socket ---

sub _daemon_request {
    my ($path, $params) = @_;

    my $query = join('&', map { "$_=" . _uri_encode($params->{$_}) } keys %$params);
    my $url = "http://localhost${path}?${query}";

    my $sock = IO::Socket::UNIX->new(
        Peer => $SOCKET_PATH,
        Type => SOCK_STREAM,
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
    return decode_json($body // '{}');
}

sub _uri_encode {
    my ($str) = @_;
    $str =~ s/([^A-Za-z0-9\-_.~])/sprintf("%%%02X", ord($1))/ge;
    return $str;
}

# --- Plugin Interface Methods ---

sub on_add_hook {
    my ($class, $storeid, $scfg, %param) = @_;

    my $access_key = delete $scfg->{'access-key'}
        or die "access-key is required\n";
    my $secret_key = delete $scfg->{'secret-key'}
        or die "secret-key is required\n";

    _write_credentials($storeid, $access_key, $secret_key);
    return;
}

sub on_update_hook {
    my ($class, $storeid, $scfg, %param) = @_;

    # Update credentials if provided
    my $access_key = delete $scfg->{'access-key'};
    my $secret_key = delete $scfg->{'secret-key'};

    if ($access_key && $secret_key) {
        _write_credentials($storeid, $access_key, $secret_key);
    }
    return;
}

sub on_delete_hook {
    my ($class, $storeid, $scfg) = @_;

    my $path = _cred_path($storeid);
    unlink $path if -f $path;
    return;
}

sub activate_storage {
    my ($class, $storeid, $scfg, $cache) = @_;

    # Ensure the daemon is running and the storage is reachable
    eval {
        my $status = _daemon_request('/v1/status', { storage => $storeid });
        die "storage $storeid is offline\n" unless $status->{online};
    };
    if ($@) {
        die "Failed to activate S3 storage '$storeid': $@\n";
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
        return ($res->{total} // 0, $res->{available} // 0, 0, 0);
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
        next if $@ || !$list;

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

    # volname is like "iso/debian-12.iso"
    my $content = 'iso';
    my $filename = $volname;
    if ($volname =~ m|^([^/]+)/(.+)$|) {
        $content  = $1;
        $filename = $2;
    }

    my $prefix = _content_to_prefix($content);
    my $key = "${prefix}${filename}";

    my $res = eval {
        _daemon_request('/v1/path', { storage => $storeid, key => $key });
    };
    if ($@ || !$res->{path}) {
        die "Failed to resolve path for $volname: $@\n";
    }

    return ($res->{path}, $content, 'raw');
}

sub _content_to_prefix {
    my ($content) = @_;
    my %map = (
        iso      => 'template/iso/',
        vztmpl   => 'template/cache/',
        snippets => 'snippets/',
        backup   => 'dump/',
    );
    return $map{$content} // "${content}/";
}

sub clone_image {
    die "clone not supported on S3 storage\n";
}

sub alloc_image {
    die "image allocation not supported on S3 storage\n";
}

sub free_image {
    my ($class, $storeid, $scfg, $volname, $isBase) = @_;

    my $content = 'iso';
    my $filename = $volname;
    if ($volname =~ m|^([^/]+)/(.+)$|) {
        $content  = $1;
        $filename = $2;
    }

    my $prefix = _content_to_prefix($content);
    my $key = "${prefix}${filename}";

    _daemon_request('/v1/delete', { storage => $storeid, key => $key });
    return;
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
