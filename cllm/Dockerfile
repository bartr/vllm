FROM golang:1.22.2-alpine AS build

WORKDIR /src

COPY go.mod ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags='-s -w' -o /out/cllm ./cmd/cllm

FROM gcr.io/distroless/static-debian12:nonroot

WORKDIR /

COPY --from=build /out/cllm /cllm

EXPOSE 8080

ENTRYPOINT ["/cllm"]
