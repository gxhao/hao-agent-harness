package config

import (
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
)

const maxDotEnvBytes = 1 << 20

var dotEnvName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// LoadDotEnv 将指定文件作为进程环境的补充来源，供命令行登录和服务启动共用。
// 缺失文件允许纯环境部署；LookupEnv 保留显式空值，避免意外重新启用已关闭的配置。
// 先完整解析再写入，格式错误不会留下部分配置；错误不包含原始行或密钥值。
func LoadDotEnv(path string) error {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf(".env: cannot read file")
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxDotEnvBytes+1))
	if err != nil {
		return fmt.Errorf(".env: cannot read file")
	}
	if len(data) > maxDotEnvBytes {
		return fmt.Errorf(".env: file exceeds 1 MiB limit")
	}
	values, err := parseDotEnv(string(data))
	if err != nil {
		return err
	}
	for name, value := range values {
		if _, exists := os.LookupEnv(name); exists {
			continue
		}
		if err := os.Setenv(name, value); err != nil {
			return fmt.Errorf(".env: cannot set environment variable")
		}
	}
	return nil
}

// parseDotEnv 只解析单行赋值，不进行变量展开或命令执行。
// 同名配置最后一次声明生效；限制为单行使错误定位和跨平台行为保持明确。
func parseDotEnv(data string) (map[string]string, error) {
	values := make(map[string]string)
	for i, raw := range strings.Split(strings.TrimPrefix(data, "\ufeff"), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "export ") || strings.HasPrefix(line, "export\t") {
			line = strings.TrimSpace(line[len("export"):])
		}
		name, value, found := strings.Cut(line, "=")
		name = strings.TrimSpace(name)
		if !found || !dotEnvName.MatchString(name) {
			return nil, fmt.Errorf(".env: line %d: invalid assignment", i+1)
		}
		parsed, err := parseDotEnvValue(strings.TrimSpace(value))
		if err != nil {
			return nil, fmt.Errorf(".env: line %d: %s", i+1, err)
		}
		// 环境字符串不能包含 NUL，提前检查避免注入阶段才失败。
		if strings.ContainsRune(parsed, 0) {
			return nil, fmt.Errorf(".env: line %d: invalid value", i+1)
		}
		values[name] = parsed
	}
	return values, nil
}

// parseDotEnvValue 保留引号内的 # 和空白；仅双引号识别约定的转义。
// 未加引号的 # 只有在行首或空白后才是注释，令牌内的 # 不会被截断。
func parseDotEnvValue(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	quote := value[0]
	if quote != '\'' && quote != '"' {
		for i := 0; i < len(value); i++ {
			if value[i] == '#' && (i == 0 || value[i-1] == ' ' || value[i-1] == '\t') {
				return strings.TrimSpace(value[:i]), nil
			}
		}
		return value, nil
	}
	var result strings.Builder
	for i := 1; i < len(value); i++ {
		c := value[i]
		if c == quote {
			tail := strings.TrimSpace(value[i+1:])
			if tail != "" && !strings.HasPrefix(tail, "#") {
				return "", fmt.Errorf("unexpected content after quote")
			}
			return result.String(), nil
		}
		if quote == '"' && c == '\\' {
			i++
			if i == len(value) {
				break
			}
			switch value[i] {
			case '\\', '"':
				c = value[i]
			case 'n':
				c = '\n'
			case 'r':
				c = '\r'
			case 't':
				c = '\t'
			default:
				return "", fmt.Errorf("unsupported escape")
			}
		}
		result.WriteByte(c)
	}
	return "", fmt.Errorf("unclosed quote; multiline values are unsupported")
}
