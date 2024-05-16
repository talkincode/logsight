package assets

import (
	"embed"
	"regexp"
)

//go:embed static
var StaticFs embed.FS

//go:embed templates
var TemplatesFs embed.FS

//go:embed buildinfo.txt
var BuildInfo string

//go:embed menu-admin.json
var AdminMenudata []byte

//go:embed menu-opr.json
var OprMenudata []byte

//go:embed pgdump_script.sh
var PgdumpShell string

var defaultBuildVer = "Latest Build 2024"

func BuildVersion() string {
	re, err := regexp.Compile(`BuildVersion=(.+?)\n`)
	if err != nil {
		return defaultBuildVer
	}
	match := re.FindStringSubmatch(BuildInfo)

	if len(match) > 0 {
		return match[1]
	}
	return defaultBuildVer
}

func BuildInfoMap() map[string]string {
	re, err := regexp.Compile(`(.+?)=(.+?)\n`)
	if err != nil {
		return nil
	}
	match := re.FindAllStringSubmatch(BuildInfo, -1)
	if len(match) > 0 {
		m := make(map[string]string)
		for _, v := range match {
			m[v[1]] = v[2]
		}
		return m
	}
	return nil
}
