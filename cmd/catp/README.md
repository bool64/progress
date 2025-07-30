# catp

`catp` is a flavour of `cat` with `STDERR` progress printer.

## Install

```
go install github.com/bool64/progress/cmd/catp@latest
```

or download from [releases](https://github.com/bool64/progress/releases).

```
wget https://github.com/bool64/progress/releases/latest/download/linux_amd64.tar.gz && tar xf linux_amd64.tar.gz && rm linux_amd64.tar.gz
./catp -version
```

## Usage

```
catp dev, go1.22rc1 CGO_ZSTD

catp prints contents of files to STDOUT or dir/file output,
while printing current progress status to STDERR.
It can decompress data from .gz and .zst files.
Use dash (-) as PATH to read STDIN.

Usage of catp:
catp [OPTIONS] PATH ...
  -dbg-cpu-prof string
    	write first 10 seconds of CPU profile to file
  -dbg-mem-prof string
    	write heap profile to file after 10 seconds
  -no-progress
    	disable progress printing
  -out-dir string
    	output to directory instead of STDOUT
    	files will be written to out dir with original base names
    	disables output flag
  -output string
    	output to file (can have .gz or .zst ext for compression) instead of STDOUT
  -parallel int
    	number of parallel readers if multiple files are provided
    	lines from different files will go to output simultaneously (out of order of files, but in order of lines in each file)
    	use 0 for multi-threaded zst decoder (slightly faster at cost of more CPU) (default 1)
  -pass value
    	filter matching, may contain multiple AND patterns separated by ^,
    	if filter matches, line is passed to the output (unless filtered out by -skip)
    	each -pass value is added with OR logic,
    	for example, you can use "-pass bar^baz -pass foo" to only keep lines that have (bar AND baz) OR foo
  -progress-json string
    	write current progress to a file
  -skip value
    	filter matching, may contain multiple AND patterns separated by ^,
    	if filter matches, line is removed from the output (even if it passed -pass)
    	each -skip value is added with OR logic,
    	for example, you can use "-skip quux^baz -skip fooO" to skip lines that have (quux AND baz) OR fooO
  -version
    	print version and exit
```

## Examples

Feed a file into `jq` field extractor with progress printing.

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

Run log filtering (lines containing `foo bar` or `baz`) on multiple files in background (with `screen`) and output to a
new file.

```
screen -dmS foo12 ./catp -output ~/foo-2023-07-12.log -pass "foo bar" -pass "baz" /home/logs/server-2023-07-12*
```

```
# attaching to screen
screen -r foo12

....
all: 32.0% bytes read, /home/logs/server-2023-07-12-08-00.log_6.zst: 96.1% bytes read, 991578374 lines processed, 447112.5 l/s, 103.0 MB/s, elapsed 36m57.74s, remaining 1h18m36s, matches 6343
all: 32.0% bytes read, /home/logs/server-2023-07-12-08-00.log_6.zst: 97.9% bytes read, 993652260 lines processed, 447039.6 l/s, 102.9 MB/s, elapsed 37m2.74s, remaining 1h18m32s, matches 6357
all: 32.1% bytes read, /home/logs/server-2023-07-12-08-00.log_6.zst: 99.6% bytes read, 995708943 lines processed, 446959.5 l/s, 102.9 MB/s, elapsed 37m7.74s, remaining 1h18m29s, matches 6372
all: 32.1% bytes read, /home/logs/server-2023-07-12-08-00.log_6.zst: 100.0% bytes read, 996191417 lines processed, 446945.2 l/s, 102.9 MB/s, elapsed 37m8.89s, remaining 1h18m28s, matches 6376
all: 32.2% bytes read, /home/logs/server-2023-07-12-09-00.log_6.zst: 1.7% bytes read, 998243047 lines processed, 446595.4 l/s, 102.8 MB/s, elapsed 37m15.23s, remaining 1h18m27s, matches 6402
all: 32.3% bytes read, /home/logs/server-2023-07-12-09-00.log_6.zst: 3.5% bytes read, 1000516319 lines processed, 446613.3 l/s, 102.8 MB/s, elapsed 37m20.23s, remaining 1h18m22s, matches 6425
all: 32.3% bytes read, /home/logs/server-2023-07-12-09-00.log_6.zst: 5.1% bytes read, 1002520923 lines processed, 446510.7 l/s, 102.8 MB/s, elapsed 37m25.23s, remaining 1h18m18s, matches 6450
....

# detaching from screen with ctrl+a+d
```

