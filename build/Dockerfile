# Build stage
FROM golang:1.22.2-alpine3.19 AS build-env
ARG PLUGIN_PATH=github.com/nxsre/k8s-ds-framework

RUN apk add curl git
WORKDIR ${GOPATH}/src/${PLUGIN_PATH}
ADD go.* ./
RUN go mod download
ADD . ./
RUN CGO_ENABLED=0 GOOS=linux go build -a -ldflags '-extldflags "-static"' -o ds-processer ${PLUGIN_PATH}/cmd/ds-processer


# Final image creation
FROM alpine:latest

ARG PLUGIN_PATH=github.com/nxsre/k8s-ds-framework
RUN apk add util-linux
COPY --from=build-env /go/src/${PLUGIN_PATH}/ds-processer /

ENTRYPOINT ["/ds-processer"]
