FROM public.ecr.aws/docker/library/golang:alpine AS builder
RUN apk add git
WORKDIR /app
ENV CGO_ENABLED=0 GOFLAGS=-trimpath
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -ldflags='-s -w' -o wspush

FROM scratch
COPY --from=builder /app/wspush /
CMD ["/wspush"]
