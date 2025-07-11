FROM golang:1.24.2 as builder
RUN apt update && apt -y install unzip wget curl libimobiledevice-utils libimobiledevice6 usbmuxd iproute2 net-tools \
        curl git build-essential libssl-dev zlib1g-dev socat && rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY . .

RUN go build -o client
RUN chmod +x client

#FROM scratch
#COPY --from=builder /app/client /client
ENTRYPOINT ["./client"]
