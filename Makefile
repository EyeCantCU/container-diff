# Copyright 2018 Google, Inc. All rights reserved.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# On release, remember to also bump const in version/version.go
GIT_VERSION ?= $(shell git describe --always --tags --long --dirty)

GOOS ?= $(shell go env GOOS)
GOARCH = amd64
BUILD_DIR ?= ./out
ORG := github.com/EyeCantCU
PROJECT := container-diff
REPOPATH ?= $(ORG)/$(PROJECT)
RELEASE_BUCKET ?= $(PROJECT)

SUPPORTED_PLATFORMS := linux-$(GOARCH) darwin-$(GOARCH) windows-$(GOARCH).exe
BUILD_PACKAGE = $(REPOPATH)

# These build tags are from the containers/image library.
#
# container_image_ostree_stub allows building the library without requiring the libostree development libraries
# container_image_openpgp forces a Golang-only OpenPGP implementation for signature verification instead of the default cgo/gpgme-based implementation
GO_BUILD_TAGS := "container_image_ostree_stub containers_image_openpgp"
GO_LDFLAGS := "-X $(REPOPATH)/version.gitVersion=$(GIT_VERSION)"
GO_FILES := $(shell go list  -f '{{join .Deps "\n"}}' $(BUILD_PACKAGE) | grep $(ORG) | xargs go list -f '{{ range $$file := .GoFiles }} {{$$.Dir}}/{{$$file}}{{"\n"}}{{end}}')

$(BUILD_DIR)/$(PROJECT): $(BUILD_DIR)/$(PROJECT)-$(GOOS)-$(GOARCH)
	cp $(BUILD_DIR)/$(PROJECT)-$(GOOS)-$(GOARCH) $@

$(BUILD_DIR)/$(PROJECT)-%-$(GOARCH): $(GO_FILES) $(BUILD_DIR)
	GOOS=$* GOARCH=$(GOARCH) CGO_ENABLED=0 go build -tags $(GO_BUILD_TAGS) -ldflags $(GO_LDFLAGS) -o $@ $(BUILD_PACKAGE)

%.sha256: %
	shasum -a 256 $< &> $@

%.exe: %
	mv $< $@

$(BUILD_DIR):
	mkdir -p $(BUILD_DIR)

.PRECIOUS: $(foreach platform, $(SUPPORTED_PLATFORMS), $(BUILD_DIR)/$(PROJECT)-$(platform))

.PHONY: cross
cross: $(foreach platform, $(SUPPORTED_PLATFORMS), $(BUILD_DIR)/$(PROJECT)-$(platform).sha256)

.PHONY: test
test: $(BUILD_DIR)/$(PROJECT)
	@ ./test.sh

.PHONY: integration
integration: $(BUILD_DIR)/$(PROJECT)
	go test -v -tags integration $(REPOPATH)/tests -timeout 20m

.PHONY: snapshot
snapshot: ## Run Goreleaser in snapshot mode
	LDFLAGS=$(GO_LDFLAGS) goreleaser release --clean --snapshot --skip=sign,publish

.PHONY: release
release: ## Run Goreleaser in release mode
	LDFLAGS=$(GO_LDFLAGS) goreleaser release --clean

.PHONY: clean
clean:
	rm -rf $(BUILD_DIR)
