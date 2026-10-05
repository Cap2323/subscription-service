FROM golang:1.27.1-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download && go mod verify
COPY cmd ./cmd
COPY internal ./internal
COPY migrations ./migrations
RUN CGO_ENABLED=0 go build -mod=readonly -trimpath -ldflags="-s -w" -o /out/api ./cmd/api && \
    CGO_ENABLED=0 go build -mod=readonly -trimpath -ldflags="-s -w" -o /out/admin ./cmd/admin

FROM scratch AS runtime
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
WORKDIR /app
COPY --from=build /out/api /app/api
USER 65532:65532
EXPOSE 8080 9090
ENTRYPOINT ["/app/api"]

FROM scratch AS admin
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
WORKDIR /app
COPY --from=build /out/admin /app/admin
USER 65532:65532
ENTRYPOINT ["/app/admin"]
