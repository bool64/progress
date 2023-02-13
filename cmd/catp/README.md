# catp

`catp` is a flavour of `cat` with `STDERR` progress printer.

## Install

```
go install github.com/bool64/progress/cmd/catp@latest
```

## Usage

```
catp get-key.log | jq .context.callback.Data.Nonce > get-key.jq
```
```
get-key.log: 4.0% bytes read, 39856 lines processed, 7968.7 l/s, 41.7 MB/s, elapsed 5s, remaining 1m59s
get-key.log: 8.1% bytes read, 79832 lines processed, 7979.9 l/s, 41.8 MB/s, elapsed 10s, remaining 1m54s
.....
get-key.log: 96.8% bytes read, 967819 lines processed, 8064.9 l/s, 41.8 MB/s, elapsed 2m0s, remaining 3s
get-key.log: 100.0% bytes read, 1000000 lines processed, 8065.7 l/s, 41.8 MB/s, elapsed 2m3.98s, remaining 0s
```