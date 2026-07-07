FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /gateway ./cmd/gateway

FROM gcr.io/distroless/static:nonroot
COPY --from=build /gateway /gateway
COPY --from=build /src/config.yaml /config.yaml
ENTRYPOINT ["/gateway"]
CMD ["-config", "/config.yaml"]
