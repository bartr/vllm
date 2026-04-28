FROM golang:1.26.2-alpine AS build

WORKDIR /src

COPY go.mod ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags='-s -w' -o /out/cllm ./cmd/cllm
RUN mkdir -p /out/var/lib/cllm && cp /src/cache.json /out/var/lib/cllm/cache.json
RUN mkdir -p /out/configs && cp /src/configs/profiles.yaml /out/configs/profiles.yaml && cp /src/configs/classes.yaml /out/configs/classes.yaml

FROM gcr.io/distroless/static-debian12:nonroot

WORKDIR /

COPY --from=build /out/cllm /cllm
COPY --from=build --chown=nonroot:nonroot /out/var/lib/cllm /var/lib/cllm
COPY --from=build /out/configs /configs

EXPOSE 8080

ENTRYPOINT ["/cllm"]
