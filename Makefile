.PHONY: build clean deb install test

BINARY := proxs3d
VERSION := 0.1.0

build:
	go build -o $(BINARY) ./cmd/proxs3d

test:
	go test ./...

clean:
	rm -f $(BINARY)
	rm -rf _build

install: build
	install -D -m 0755 $(BINARY) /usr/bin/$(BINARY)
	install -D -m 0644 perl/S3Plugin.pm /usr/share/perl5/PVE/Storage/Custom/S3Plugin.pm
	install -D -m 0644 systemd/proxs3d.service /lib/systemd/system/proxs3d.service
	install -d -m 0755 /var/cache/proxs3
	@if [ ! -f /etc/proxs3/proxs3d.json ]; then \
		install -D -m 0640 examples/proxs3d.json /etc/proxs3/proxs3d.json; \
	fi

deb:
	dpkg-buildpackage -us -uc -b
