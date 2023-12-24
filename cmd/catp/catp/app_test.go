package catp_test

import (
	"os"
	"testing"

	"github.com/bool64/progress/cmd/catp/catp"
)

func Test_Main(t *testing.T) {
	os.Args = []string{
		"catp",
		"-pass", "linux^64",
		"-pass", "windows",
		"-skip", "dbg",
		"-output", "testdata/filtered.log",
		"testdata/release-assets.yml",
	}

	if err := catp.Main(); err != nil {
		t.Fatal(err)
	}

	d, err := os.ReadFile("testdata/filtered.log")
	if err != nil {
		t.Fatal(err)
	}

	if string(d) != `          curl -sL https://storage.googleapis.com/go-build-snap/go/linux-amd64/$(git ls-remote https://github.com/golang/go.git HEAD | awk '{print $1;}').tar.gz -o gotip.tar.gz
      - name: Upload linux_amd64.tar.gz
        if: hashFiles('linux_amd64.tar.gz') != ''
          asset_path: ./linux_amd64.tar.gz
          asset_name: linux_amd64.tar.gz
      - name: Upload linux_arm64.tar.gz
        if: hashFiles('linux_arm64.tar.gz') != ''
          asset_path: ./linux_arm64.tar.gz
          asset_name: linux_arm64.tar.gz
      - name: Upload windows_amd64.zip
        if: hashFiles('windows_amd64.zip') != ''
          asset_path: ./windows_amd64.zip
          asset_name: windows_amd64.zip
` {
		t.Fatal("Unexpected output:\n", string(d))
	}
}
