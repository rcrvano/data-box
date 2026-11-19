# syntax=docker/dockerfile:1
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/data-box .

FROM alpine:3.22
RUN apk add --no-cache ca-certificates tzdata \
 && adduser -D -u 10001 databox && mkdir -p /data && chown databox /data
COPY --from=build /out/data-box /usr/local/bin/data-box
USER databox
ENV DATA_BOX_DIR=/data DATA_BOX_ADDR=0.0.0.0:8790
VOLUME /data
EXPOSE 8790
HEALTHCHECK --interval=30s --timeout=3s CMD wget -qO- http://127.0.0.1:8790/healthz || exit 1
ENTRYPOINT ["data-box"]
CMD ["serve"]
