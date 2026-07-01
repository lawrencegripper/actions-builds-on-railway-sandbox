FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" \
    -o /out/railway-sandbox-actions ./cmd/railway-sandbox-actions

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/railway-sandbox-actions /usr/local/bin/railway-sandbox-actions
ENTRYPOINT ["/usr/local/bin/railway-sandbox-actions"]
