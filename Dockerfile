# golang:1.26
FROM golang@sha256:2005724102f45917a63e9d092fc0e4ea56ea575048ce147caad5f5f61502c365 AS builder

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

ENV CGO_ENABLED=0
# Building ./cmd/... drops one static binary per main package into /out.
RUN go build -trimpath -ldflags="-s -w" -o /out/ ./cmd/...

# gcr.io/distroless/static-debian12:nonroot
FROM gcr.io/distroless/static-debian12@sha256:f5b485ea962d9bd1186b2f6b3a061191539b905b82ec395de78cbfae51f20e35

COPY --from=builder /out/penstock /penstock
COPY --from=builder /out/llmsim /llmsim

# Inherited from the nonroot image, restated so a digest bump cannot silently
# drop it.
USER nonroot:nonroot

EXPOSE 8080

ENTRYPOINT ["/penstock"]
