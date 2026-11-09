VERSION := $(if $(VERSION),$(VERSION),"0.0.0-dev")

all: auth.go main.go
	go build -ldflags "-X main.AppVersion=$(VERSION)" -o spotify-cli main.go auth.go

clean:
	rm -rf deb
	rm *.deb

ARCH := $(if $(ARCH),$(ARCH),$(shell dpkg --print-architecture))
deb: all
	rm -rf deb
	mkdir -p deb/spotify-cli/usr/bin
	mkdir deb/spotify-cli/DEBIAN
	cp deb.control deb/spotify-cli/DEBIAN/control
	sed -i'' "s/__version__/$(VERSION)/" deb/spotify-cli/DEBIAN/control
	sed -i'' "s/__arch__/$(ARCH)/" deb/spotify-cli/DEBIAN/control
	cat deb/spotify-cli/DEBIAN/control
	cp spotify-cli deb/spotify-cli/usr/bin
	dpkg-deb -Zgzip --root-owner-group --build deb/spotify-cli
