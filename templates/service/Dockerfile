FROM golang:1.27.3-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/starter ./cmd/starter

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/starter /starter
ENV GOMEMLIMIT=48MiB
USER 1000:1000
EXPOSE 8080 9100
HEALTHCHECK --interval=10s --timeout=5s --start-period=5s --retries=3 CMD ["/starter", "-healthcheck"]
ENTRYPOINT ["/starter"]
