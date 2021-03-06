# first image used to build the sources
FROM golang:1.15.5-buster AS builder

ENV GO111MODULE=on \
    GOOS=linux \
    CGO_ENABLED=1 \
    GOARCH=amd64

WORKDIR /tdex-daemon

COPY go.mod .
COPY go.sum .
RUN go mod download

COPY . .

RUN go build -ldflags="-s -w" -o tdexd-linux cmd/tdexd/main.go
RUN go build -ldflags="-s -w" -o tdex cmd/tdex/*

WORKDIR /build

RUN cp /tdex-daemon/tdexd-linux .
RUN cp /tdex-daemon/tdex .

# Second image, running the tdexd executable
FROM debian:buster

# TDEX environment variables with default values
ENV TDEX_TRADER_LISTENING_PORT=9945
ENV TDEX_OPERATOR_LISTENING_PORT=9000
ENV TDEX_EXPLORER_ENDPOINT="http://127.0.0.1:3001"
ENV TDEX_DATA_DIR_PATH="/.tdex-daemon"
ENV TDEX_LOG_LEVEL=5
ENV TDEX_DEFAULT_FEE=0.25
ENV TDEX_NETWORK="regtest"
ENV TDEX_BASE_ASSET="5ac9f65c0efcc4775e0baec4ec03abdde22473cd3cf33c0419ca290e0751b225"
ENV TDEX_CRAWL_INTERVAL=1000
ENV TDEX_FEE_ACCOUNT_BALANCE_TRESHOLD=1000
ENV TDEX_TRADE_EXPIRY_TIME=120
ENV TDEX_PRICE_SLIPPAGE=0.05
ENV TDEX_MNEMONIC=""
ENV TDEX_UNSPENT_TTL=120

COPY --from=builder /build/tdexd-linux /
COPY --from=builder /build/tdex /

RUN install /tdex /bin

# expose trader and operator interface ports
EXPOSE 9945
EXPOSE 9000

CMD /tdexd-linux

