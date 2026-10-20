package main

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/term"
)

// credentials are resolved from flags, then the environment, then the config
// file, and finally an interactive prompt when stdin is a terminal.
type credentials struct {
	User     string
	Password string
}

func defaultConfigPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "bauduu", "config")
}

// readConfig parses a key=value file; blank lines and # comments are ignored.
// Values are trimmed unless double-quoted (Go syntax), which keeps leading or
// trailing spaces and escapes intact. A missing file is not an error.
func readConfig(path string) (map[string]string, error) {
	values := map[string]string{}
	if path == "" {
		return values, nil
	}
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return values, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("%s:%d: expected key=value", path, n)
		}
		value = strings.TrimSpace(value)
		if strings.HasPrefix(value, `"`) {
			if value, err = strconv.Unquote(value); err != nil {
				return nil, fmt.Errorf("%s:%d: bad quoted value: %w", path, n, err)
			}
		}
		values[strings.ToLower(strings.TrimSpace(key))] = value
	}
	return values, sc.Err()
}

func writeConfig(path string, c credentials) error {
	if path == "" {
		return errors.New("no config path available")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	// Quote both values so any character, spaces at the edges included,
	// survives the round trip through readConfig.
	content := fmt.Sprintf("# bauduu-cli credentials\nuser = %s\npassword = %s\n", strconv.Quote(c.User), strconv.Quote(c.Password))
	// Write a fresh 0600 file (CreateTemp's mode) and rename it into place, so
	// the password never sits in an existing file with looser permissions.
	f, err := os.CreateTemp(dir, ".config-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.WriteString(content); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

func resolveCredentials(flagUser, configPath string, interactive bool) (credentials, error) {
	c := credentials{User: flagUser, Password: os.Getenv("BAUDUU_PASSWORD")}
	if c.User == "" {
		c.User = os.Getenv("BAUDUU_USER")
	}
	// The config file is only a fallback; explicit credentials must work even
	// when it is broken.
	if c.User == "" || c.Password == "" {
		cfg, err := readConfig(configPath)
		if err != nil {
			return c, err
		}
		if c.User == "" {
			c.User = cfg["user"]
		}
		// Only use the stored password for the stored user.
		if c.Password == "" && (c.User == "" || strings.EqualFold(c.User, cfg["user"])) {
			c.Password = cfg["password"]
		}
	}
	if !interactive || !term.IsTerminal(int(os.Stdin.Fd())) {
		if c.User == "" || c.Password == "" {
			return c, errors.New("no credentials: set BAUDUU_USER/BAUDUU_PASSWORD, run 'bauduu login --save', or use a terminal")
		}
		return c, nil
	}
	if c.User == "" {
		fmt.Fprint(os.Stderr, "Benutzername: ")
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && line == "" {
			return c, err
		}
		c.User = strings.TrimSpace(line)
	}
	if c.Password == "" {
		fmt.Fprintf(os.Stderr, "Passwort für %s: ", c.User)
		pw, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return c, err
		}
		c.Password = string(pw)
	}
	if c.User == "" || c.Password == "" {
		return c, errors.New("username and password are required")
	}
	return c, nil
}
