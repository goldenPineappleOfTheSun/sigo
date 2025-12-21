FROM golang:1.25-alpine

WORKDIR /app

COPY . .

WORKDIR /app/server
RUN go build -o server

CMD ["./server"]