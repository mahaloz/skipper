from golang:1.17 as builder

COPY . /go/skipper

RUN apt update && apt install clang -y

# install source of target
RUN mkdir ~/gopath && \
    export GOPATH="$HOME/gopath" && \
    export PATH="$PATH:$GOPATH/bin" && \
    cd /go/skipper/eskip && \
    go install -tags production github.com/zalando/skipper && \
    go get github.com/dvyukov/go-fuzz/go-fuzz github.com/dvyukov/go-fuzz/go-fuzz-build && \
    go-fuzz-build -libfuzzer -o fuzz_eskip_proto_parser.a . && \
    clang -fsanitize=fuzzer fuzz_eskip_proto_parser.a  -o fuzz_eskip_proto_parser && \
    cp fuzz_eskip_proto_parser /fuzz_eskip_proto_parser

FROM golang:1.17
COPY --from=builder /fuzz_eskip_proto_parser /
