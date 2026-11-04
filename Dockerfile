FROM golang:1.26 AS builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .

ARG VERSION=dev
RUN CGO_ENABLED=0 go build -ldflags="-s -w -extldflags=-static -X main.version=${VERSION}" -trimpath -o /bin/nodepartition-controller ./cmd/nodepartition-controller

FROM gcr.io/distroless/static:nonroot
COPY --from=builder /bin/nodepartition-controller /usr/local/bin/nodepartition-controller
ENTRYPOINT ["nodepartition-controller"]
