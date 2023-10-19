FROM public.ecr.aws/docker/library/golang:alpine AS builder
WORKDIR /app
ENV GOFLAGS="-ldflags=-w -trimpath" CGO_ENABLED=0
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -ldflags='-s -w' -o wspush

FROM scratch
COPY --from=builder /app/wspush /
CMD ["/wspush"]
