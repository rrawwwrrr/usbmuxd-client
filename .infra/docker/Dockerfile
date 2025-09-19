FROM docker-registry.x5.ru/golang:1.25 as builder

ENV GOPROXY="https://art.x5.ru/artifactory/api/go/go"

# Обновляем список источников
RUN rm -rf /etc/apt/sources.list && \
  echo "deb https://art.x5.ru/artifactory/deb-debian-remote bullseye main" >> /etc/apt/sources.list && \
  echo "deb https://art.x5.ru/artifactory/deb-debian-remote bullseye-updates main" >> /etc/apt/sources.list && \
  echo "deb https://art.x5.ru/artifactory/deb-debian-security-remote bullseye-security main" >> /etc/apt/sources.list

# Добавляем корпоративные сертификаты
COPY --from=docker-base-images-prod.x5.ru/x5-certs:latest /x5/certs/* /usr/local/share/ca-certificates/
# Устанавливаем необходимые пакеты
ENV DEBIAN_FRONTEND=noninteractive
#RUN apt update -o "Acquire::https::Verify-Peer=false" -y && \
#    apt install -o "Acquire::https::Verify-Peer=false" -y ca-certificates && \
#    update-ca-certificates
#
#RUN apt-get update && apt-get -y install unzip wget curl git

WORKDIR /app
COPY . .
RUN go build -o client
RUN chmod +x client

FROM docker-registry.x5.ru/debian:bullseye-slim

# Обновляем список источников
RUN rm -rf /etc/apt/sources.list && \
  echo "deb https://art.x5.ru/artifactory/deb-debian-remote bullseye main" >> /etc/apt/sources.list && \
  echo "deb https://art.x5.ru/artifactory/deb-debian-remote bullseye-updates main" >> /etc/apt/sources.list && \
  echo "deb https://art.x5.ru/artifactory/deb-debian-security-remote bullseye-security main" >> /etc/apt/sources.list

# Добавляем корпоративные сертификаты
COPY --from=docker-base-images-prod.x5.ru/x5-certs:latest /x5/certs/* /usr/local/share/ca-certificates/
# Устанавливаем необходимые пакеты
ENV DEBIAN_FRONTEND=noninteractive
RUN apt update -o "Acquire::https::Verify-Peer=false" -y && \
    apt install -o "Acquire::https::Verify-Peer=false" -y ca-certificates && \
    update-ca-certificates

RUN apt-get update && apt-get -y install unzip wget curl libimobiledevice-utils libimobiledevice6 usbmuxd iproute2 net-tools \
    curl git build-essential libssl-dev zlib1g-dev socat && rm -rf /var/run/usbmuxd
WORKDIR /app
COPY --from=builder /app/client /client
RUN groupadd -g 1301 androidusr && useradd -u 1300 -g 1301 androidusr && chmod +x /client && chown androidusr:androidusr /client
USER androidusr
ENTRYPOINT ["/client"]