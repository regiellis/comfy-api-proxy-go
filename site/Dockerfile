# The docs site as a static binary on Alpine. Build from the repository root,
# so the image can copy docs/*.md into the module before go:embed reads it:
#   docker build -f site/Dockerfile .
FROM golang:1.26.10-alpine3.24 AS builder
WORKDIR /src
COPY site/go.mod site/go.sum ./
RUN go mod download
COPY site/*.go ./
COPY site/static ./static
COPY site/templates ./templates
COPY docs/*.md ./docs/
ARG RELEASE=development
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.releaseVersion=${RELEASE}" -o /out/site .

FROM alpine:3.24.3
RUN adduser -D -H -u 10001 site
COPY --from=builder /out/site /app/site
USER 10001:10001
# PUBLIC_ORIGIN has no default: production refuses to start without it.
ENV HOST=0.0.0.0 PORT=8080 ENVIRONMENT=production
EXPOSE 8080
# BusyBox wget is in the base image. Loopback passes the site's Host check.
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
  CMD wget -q -O /dev/null http://127.0.0.1:8080/healthz || exit 1
ENTRYPOINT ["/app/site"]
