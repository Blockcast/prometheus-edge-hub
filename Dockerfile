# Copyright (c) Facebook, Inc. and its affiliates.
#
# This source code is licensed under the MIT license found in the
# LICENSE file in the root directory of this source tree.

# Go >= 1.26.10 and Alpine >= 3.23 clear the Harbor findings in BLO-57022.
FROM golang:1.26.11-alpine3.23 AS go

# Use public go modules proxy
ENV GOPROXY https://proxy.golang.org

WORKDIR /app/prometheus-edge-hub

COPY go.mod .
COPY go.sum .

RUN go mod download

COPY . .

RUN go build -o /build/bin/prometheus-edge-hub

FROM alpine:3.23

COPY --from=go /build/bin/prometheus-edge-hub /bin/prometheus-edge-hub

EXPOSE 9091

ENTRYPOINT ["prometheus-edge-hub"]
