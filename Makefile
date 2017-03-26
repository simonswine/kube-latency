ACCOUNT=simonswine
APP_NAME=kube-latency

PACKAGE_NAME=github.com/${ACCOUNT}/${APP_NAME}
GO_VERSION=1.8

DOCKER_IMAGE=${ACCOUNT}/${APP_NAME}

BUILD_DIR=_build

CONTAINER_DIR=/go/src/${PACKAGE_NAME}

.PHONY: version

all: build

depend:
	rm -rf ${BUILD_DIR}/
	mkdir $(BUILD_DIR)/

version:
	$(eval GIT_STATE := $(shell if test -z "`git status --porcelain 2> /dev/null`"; then echo "clean"; else echo "dirty"; fi))
	$(eval GIT_COMMIT := $(shell git rev-parse HEAD))
	$(eval APP_VERSION := $(shell cat VERSION))

build: depend version
	CGO_ENABLED=0 GOARCH=amd64 GOOS=linux go build \
		-a -tags netgo \
		-o ${BUILD_DIR}/${APP_NAME}-linux-amd64 \
		-ldflags "-X main.AppGitState=${GIT_STATE} -X main.AppGitCommit=${GIT_COMMIT} -X main.AppVersion=${APP_VERSION}"

docker: docker_all

docker_%:
	# create a container
	$(eval CONTAINER_ID := $(shell docker create \
		-i \
		-w $(CONTAINER_DIR) \
		golang:${GO_VERSION} \
		/bin/bash -c "tar xf - && make $*" \
	))
	
	# run build inside container
	tar cf - . | docker start -a -i $(CONTAINER_ID)

	# copy artifacts over
	rm -rf $(BUILD_DIR)/
	docker cp $(CONTAINER_ID):$(CONTAINER_DIR)/$(BUILD_DIR)/ .

	# remove container
	docker rm $(CONTAINER_ID)

image: docker_all version
	docker build --build-arg VCS_REF=$(GIT_COMMIT) -t $(ACCOUNT)/$(APP_NAME):latest .
	
push: image
	docker push $(ACCOUNT)/$(APP_NAME):latest
