# Copyright 2018 Intel Corporation.
# Use of this source code is governed by a BSD-style
# license that can be found in the LICENSE file.

# --------- General build rules

ifndef NFF_GO_NO_MLX_DRIVERS
ifeq (,$(findstring mlx,$(GO_BUILD_TAGS)))
export GO_BUILD_TAGS += mlx
endif
endif

ifndef NFF_GO_NO_BPF_SUPPORT
ifeq (,$(findstring bpf,$(GO_BUILD_TAGS)))
export GO_BUILD_TAGS += bpf
endif
CFLAGS :=  $(CGO_CFLAGS) -DNFF_GO_SUPPORT_XDP
export CGO_CFLAGS = $(CFLAGS)
$(info CFLAGS = $(CGO_CFLAGS))
endif

.PHONY: all
all: nff-go-nat client/client httpperfserv wrk

.PHONY: debug
debug: | .set-debug all

.PHONY: .set-debug
.set-debug:
	$(eval GO_COMPILE_FLAGS += -gcflags=all='-N -l')

.check-downloads:
	go mod download

client/client: .check-env .check-downloads Makefile client/client.go
	cd client && go build $(GO_COMPILE_FLAGS) -tags "${GO_BUILD_TAGS}"

nff-go-nat: .check-env .check-downloads Makefile nat.go $(wildcard nat/*.go)
	go build $(GO_COMPILE_FLAGS) -tags "${GO_BUILD_TAGS}"

.PHONY: httpperfserv
httpperfserv:
	cd test/httpperfserv && go build $(GO_COMPILE_FLAGS) -tags "${GO_BUILD_TAGS}"

.PHONY: wrk
wrk:
	$(MAKE) -s -C test/wrk

.PHONY: clean
clean:
	-rm nff-go-nat
	-rm client/client
	-rm test/httpperfserv/httpperfserv
	$(MAKE) -C test/wrk clean

# --------- Docker images build rules

IMAGENAME=nff-go-nat
BASEIMAGE=nff-go-base
# Add user name to generated images
ifdef NFF_GO_IMAGE_PREFIX
WORKIMAGENAME=$(NFF_GO_IMAGE_PREFIX)/$(USER)/$(IMAGENAME)
IMAGE_PREFIX=$(NFF_GO_IMAGE_PREFIX)/$(USER)
BASEIMAGENAME=$(NFF_GO_IMAGE_PREFIX)/$(USER)/$(BASEIMAGE)
else
WORKIMAGENAME=$(USER)/$(IMAGENAME)
IMAGE_PREFIX=$(USER)
BASEIMAGENAME=$(USER)/$(BASEIMAGE)
endif

.PHONY: .check-base-image
.check-base-image:
	@if ! docker images '$(BASEIMAGENAME)' | grep '$(BASEIMAGENAME)' > /dev/null; then		\
		echo "!!! You need to build $(BASEIMAGENAME) docker image in $(NFF_GO) repository";	\
		exit 1;											\
	fi

.PHONY: images
images: .check-base-image Dockerfile all
	docker build --build-arg USER_NAME=$(IMAGE_PREFIX) -t $(WORKIMAGENAME) .

.PHONY: clean-images
clean-images: clean
	-docker rmi $(WORKIMAGENAME)

# --------- Docker deployment rules

.PHONY: .check-deploy-env
.check-deploy-env: .check-defined-NFF_GO_HOSTS

.PHONY: deploy
deploy: .check-deploy-env images
	$(eval TMPNAME=tmp-$(IMAGENAME).tar)
	docker save $(WORKIMAGENAME) > $(TMPNAME)
	for host in `echo $(NFF_GO_HOSTS) | tr ',' ' '`; do			\
		if ! docker -H tcp://$$host load < $(TMPNAME); then break; fi;	\
	done
	rm $(TMPNAME)

.PHONY: cleanall
cleanall: .check-deploy-env clean-images
	-for host in `echo $(NFF_GO_HOSTS) | tr ',' ' '`; do	\
		docker -H tcp://$$host rmi -f $(WORKIMAGENAME);	\
	done

# --------- Test execution rules

.PHONY: .check-test-env
.check-test-env: .check-defined-NFF_GO .check-defined-NFF_GO_HOSTS $(NFF_GO)/test/framework/main/tf

.PHONY: test-stability
test-stability: .check-test-env test/stability-nat.json
	$(NFF_GO)/test/framework/main/tf -directory nat-stabilityresults -config test/stability-nat.json -hosts $(NFF_GO_HOSTS)

.PHONY: test-stability-vlan
test-stability-vlan: .check-test-env test/stability-nat-vlan.json
	$(NFF_GO)/test/framework/main/tf -directory nat-vlan-stabilityresults -config test/stability-nat-vlan.json -hosts $(NFF_GO_HOSTS)

.PHONY: test-performance
test-performance: .check-test-env test/perf-nat.json
	$(NFF_GO)/test/framework/main/tf -directory nat-perfresults -config test/perf-nat.json -hosts $(NFF_GO_HOSTS)

.PHONY: test-performance-vlan
test-performance-vlan: .check-test-env test/perf-nat-vlan.json
	$(NFF_GO)/test/framework/main/tf -directory nat-vlan-perfresults -config test/perf-nat-vlan.json -hosts $(NFF_GO_HOSTS)

.PHONY: test-linux-performance
test-linux-performance: .check-test-env test/perf-nat-linux.json
	$(NFF_GO)/test/framework/main/tf -directory linux-nat-perfresults -config test/perf-nat-linux.json -hosts $(NFF_GO_HOSTS)

.PHONY: test-linux-performance-vlan
test-linux-performance-vlan: .check-test-env test/perf-nat-linux-vlan.json
	$(NFF_GO)/test/framework/main/tf -directory linux-nat-vlan-perfresults -config test/perf-nat-linux-vlan.json -hosts $(NFF_GO_HOSTS)

# --------- Utility rules

.PHONY: .check-env
.check-env: 					\
	.check-defined-RTE_TARGET		\
	.check-defined-RTE_SDK			\
	.check-defined-CGO_LDFLAGS_ALLOW	\
	.check-defined-CGO_CFLAGS		\
	.check-defined-CGO_LDFLAGS

.PHONY: .check-defined-%
.check-defined-%:
	@if [ -z '${${*}}' ]; then echo "!!! Variable $* is undefined" && exit 1; fi
