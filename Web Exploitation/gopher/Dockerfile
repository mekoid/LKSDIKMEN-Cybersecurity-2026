# Builder
FROM golang:1.22 as builder
WORKDIR /app
COPY app/go.mod ./
RUN go mod download
COPY app/ ./
# build
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /bin/app main.go
# >>> add: empty data dir for runtime writes
RUN mkdir -p /workspace/data

# Runner
FROM gcr.io/distroless/static:nonroot
WORKDIR /app
COPY --chown=nonroot:nonroot --from=builder /bin/app /app/app
COPY --chown=nonroot:nonroot app/templates /app/templates
COPY --chown=nonroot:nonroot flag.txt /flag.txt
# >>> add: bring the writable data dir
COPY --chown=nonroot:nonroot --from=builder /workspace/data /app/data
EXPOSE 13387
USER nonroot
ENTRYPOINT ["/app/app"]

