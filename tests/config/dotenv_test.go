package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"hao-agent-harness/internal/config"
)

const maxDotEnvBytes = 1 << 20

func TestParseDotEnv(t *testing.T) {
	data := "\ufeff# comment\r\n export A = first\r\nA=last # comment\nEMPTY=\n" +
		"SINGLE=' # $A \\n ' # comment\nDOUBLE=\"line\\n\\r\\t\\\"\\\\#\"\n" +
		"HASH=token#part\nCOMMENT=# empty\nLITERAL=${A} $(command)\nexport\tLOWER_name=ok\n"
	want := map[string]string{
		"A": "last", "EMPTY": "", "SINGLE": " # $A \\n ",
		"DOUBLE": "line\n\r\t\"\\#", "HASH": "token#part", "COMMENT": "",
		"LITERAL": "${A} $(command)", "LOWER_name": "ok",
	}
	// 从公开加载接口检查语法结果，避免测试依赖解析器的私有函数。
	for name := range want {
		unsetDotEnvTestVariable(t, name)
	}
	if err := config.LoadDotEnv(writeDotEnvTestFile(t, data)); err != nil {
		t.Fatal(err)
	}
	for name, expected := range want {
		if value, exists := os.LookupEnv(name); !exists || value != expected {
			t.Fatal("dotenv syntax did not produce expected values")
		}
	}
}

func TestParseDotEnvRejectsMalformedValuesWithoutDisclosure(t *testing.T) {
	for _, line := range []string{
		"NO_EQUALS", "1INVALID=private-test-value", "BAD-NAME=private-test-value",
		"A='private-test-value", "A=\"private-test-value\\q\"",
		"A='private-test-value' trailing", "A=\"private-test-value\ncontinued\"",
		"A=private-test-value\x00",
	} {
		err := config.LoadDotEnv(writeDotEnvTestFile(t, "GOOD=ok\n"+line))
		if err == nil || !strings.Contains(err.Error(), "line 2:") || strings.Contains(err.Error(), "private-test-value") {
			t.Fatal("invalid dotenv input must return a sanitized line error")
		}
	}
}

// unsetDotEnvTestVariable 保留调用前的环境，包括变量存在但为空的区别。
func unsetDotEnvTestVariable(t *testing.T, name string) {
	t.Helper()
	t.Setenv(name, "")
	if err := os.Unsetenv(name); err != nil {
		t.Fatal(err)
	}
}

func writeDotEnvTestFile(t *testing.T, data string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadDotEnvEnvironmentPrecedence(t *testing.T) {
	const missing = "HARNESS_DOTENV_TEST_MISSING"
	const existing = "HARNESS_DOTENV_TEST_EXISTING"
	const empty = "HARNESS_DOTENV_TEST_EMPTY"
	unsetDotEnvTestVariable(t, missing)
	t.Setenv(existing, "process-value")
	t.Setenv(empty, "")
	path := writeDotEnvTestFile(t, missing+"=first\n"+missing+"=last\n"+existing+"=file-value\n"+empty+"=file-value\n")
	if err := config.LoadDotEnv(path); err != nil {
		t.Fatal(err)
	}
	if os.Getenv(missing) != "last" || os.Getenv(existing) != "process-value" {
		t.Fatal("dotenv must fill missing values and preserve process values")
	}
	if value, exists := os.LookupEnv(empty); !exists || value != "" {
		t.Fatal("explicit empty process value must remain empty")
	}
}

func TestLoadDotEnvFailureDoesNotInject(t *testing.T) {
	const name = "HARNESS_DOTENV_TEST_ATOMIC"
	unsetDotEnvTestVariable(t, name)
	path := writeDotEnvTestFile(t, name+"=private-test-value\nINVALID\n")
	if err := config.LoadDotEnv(path); err == nil {
		t.Fatal("malformed file accepted")
	}
	if _, exists := os.LookupEnv(name); exists {
		t.Fatal("malformed file injected partial configuration")
	}
}

func TestLoadDotEnvFileBoundaries(t *testing.T) {
	if err := config.LoadDotEnv(filepath.Join(t.TempDir(), "missing")); err != nil {
		t.Fatal("missing file must be optional")
	}
	// 目录不能作为配置文件读取，比权限测试更适合 Windows 和高权限环境。
	if err := config.LoadDotEnv(t.TempDir()); err == nil {
		t.Fatal("unreadable file accepted")
	}
	if err := config.LoadDotEnv(writeDotEnvTestFile(t, "#"+strings.Repeat("x", maxDotEnvBytes-1))); err != nil {
		t.Fatal("file at limit must be accepted")
	}
	if err := config.LoadDotEnv(writeDotEnvTestFile(t, strings.Repeat("x", maxDotEnvBytes+1))); err == nil {
		t.Fatal("oversized file accepted")
	}
}
