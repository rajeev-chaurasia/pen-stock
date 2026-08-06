FROM golang:1.26 AS builder

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

ENV CGO_ENABLED=0
# Building ./cmd/... drops one static binary per main package into /out.
RUN go build -trimpath -ldflags="-s -w" -o /out/ ./cmd/...

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /out/penstock /penstock
COPY --from=builder /out/llmsim /llmsim

EXPOSE 8080

ENTRYPOINT ["/penstock"]
