// Command setversion keeps the "version" field of mygo.json in sync with the
// git tag a release is built from.
//
//	go run ./scripts/setversion             # use the latest v* tag
//	go run ./scripts/setversion v1.2.0      # use an explicit tag (v is optional)
//	go run ./scripts/setversion --print     # print the configured version
//
// `go tool mygo build` has no flag for the version: it always reads `version`
// from mygo.json, so a tag that reaches the release workflow would otherwise
// ship whichever version happened to be committed. The workflow runs this
// with $GITHUB_REF_NAME before building, so v1.2.0 always produces 1.2.0
// bundles. Only the version line is rewritten, which keeps release diffs
// reviewable.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
)

const configPath = "mygo.json"

// versionValue captures the value of the top-level "version" entry, so the
// file can be rewritten in place without reordering or reformatting it.
var versionValue = regexp.MustCompile(`(?m)^[ \t]*"version"[ \t]*:[ \t]*"([^"]*)"`)

// versionFormat accepts the semver forms mygo writes into bundles: 1.2.3 with
// an optional pre-release and/or build suffix.
var versionFormat = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(?:[-.+][0-9A-Za-z.-]+)?$`)

func main() {
	printOnly := flag.Bool("print", false, "print the configured version and exit")
	flag.Parse()
	if err := run(flag.Args(), *printOnly); err != nil {
		fmt.Fprintln(os.Stderr, "setversion: "+err.Error())
		os.Exit(1)
	}
}

func run(args []string, printOnly bool) error {
	src, err := os.ReadFile(configPath)
	if err != nil {
		return err
	}
	loc := versionValue.FindSubmatchIndex(src)
	if loc == nil {
		return fmt.Errorf("%s: no top-level \"version\" field", configPath)
	}
	current := string(src[loc[2]:loc[3]])

	if printOnly {
		fmt.Println(current)
		return nil
	}

	tag := ""
	if len(args) > 0 {
		tag = args[0]
	} else {
		tag, err = latestTag()
		if err != nil {
			return err
		}
	}
	version, err := normalize(tag)
	if err != nil {
		return err
	}
	if version == current {
		fmt.Printf("%s: version already %s\n", configPath, version)
		return nil
	}

	out := make([]byte, 0, len(src)+len(version))
	out = append(out, src[:loc[2]]...)
	out = append(out, version...)
	out = append(out, src[loc[3]:]...)
	if err := os.WriteFile(configPath, out, 0o644); err != nil {
		return err
	}
	fmt.Printf("%s: version %s -> %s\n", configPath, current, version)
	return nil
}

// latestTag returns the most recent v* tag reachable from HEAD.
func latestTag() (string, error) {
	out, err := exec.Command("git", "describe", "--tags", "--match", "v*", "--abbrev=0").Output()
	if err != nil {
		return "", errors.New("no v* tag found; pass a version, for example `go run ./scripts/setversion v0.2.0`")
	}
	return strings.TrimSpace(string(out)), nil
}

// normalize turns a tag such as v1.2.0 into the version 1.2.0.
func normalize(tag string) (string, error) {
	version := strings.TrimPrefix(strings.TrimSpace(tag), "v")
	if !versionFormat.MatchString(version) {
		return "", fmt.Errorf("%q is not a version (want v1.2.3)", tag)
	}
	return version, nil
}
