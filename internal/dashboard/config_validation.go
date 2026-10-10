package dashboard

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/omniswitch-dev/omniswitch/internal/gateway"
	"github.com/omniswitch-dev/omniswitch/internal/gatewayconfig"
	"go.yaml.in/yaml/v3"
)

const maxConfigBody = 1 << 20

type configInput struct {
	Format string `json:"format"`
	Body   string `json:"body"`
}

type configValidationError struct {
	Line    int    `json:"line"`
	Message string `json:"message"`
}

type configValidation struct {
	Valid  bool                    `json:"valid"`
	Errors []configValidationError `json:"errors"`
}

var yamlLine = regexp.MustCompile(`line ([0-9]+)`)

func configFormat(format string) (string, error) {
	switch strings.ToLower(strings.TrimPrefix(strings.TrimSpace(format), ".")) {
	case "", "yaml", "yml":
		return "yaml", nil
	case "json":
		return "json", nil
	default:
		return "", fmt.Errorf("format must be yaml or json")
	}
}

// validateConfig is shared by validation and raw-save. Parse performs the
// gateway's semantic validation; the extra decode refuses trailing documents
// that the normal parser would otherwise ignore.
func validateConfig(input configInput) configValidation {
	out := configValidation{Valid: true, Errors: []configValidationError{}}
	format, err := configFormat(input.Format)
	if err == nil && strings.TrimSpace(input.Body) == "" {
		err = fmt.Errorf("body is required")
	}
	if err == nil && len(input.Body) > maxConfigBody {
		err = fmt.Errorf("config body must not exceed 1 MiB")
	}
	if err == nil {
		var extra any
		if format == "json" {
			decoder := json.NewDecoder(strings.NewReader(input.Body))
			if decodeErr := decoder.Decode(&extra); decodeErr != nil {
				err = decodeErr
			} else if decodeErr := decoder.Decode(&extra); decodeErr != io.EOF {
				err = fmt.Errorf("config must contain exactly one JSON document")
			}
		} else {
			decoder := yaml.NewDecoder(strings.NewReader(input.Body))
			if decodeErr := decoder.Decode(&extra); decodeErr != nil {
				err = decodeErr
			} else if decodeErr := decoder.Decode(&extra); decodeErr != io.EOF {
				err = fmt.Errorf("config must contain exactly one YAML document")
			}
		}
	}
	if err == nil {
		_, err = gatewayconfig.Parse([]byte(input.Body), "."+format)
	}
	if err != nil {
		line := 0 // Semantic errors do not have an honest source line.
		var syntaxError *json.SyntaxError
		var yamlTypeError *yaml.TypeError
		if errors.As(err, &syntaxError) && syntaxError.Offset > 0 && syntaxError.Offset <= int64(len(input.Body)) {
			line = bytes.Count([]byte(input.Body[:syntaxError.Offset-1]), []byte("\n")) + 1
		} else if matches := yamlLine.FindStringSubmatch(err.Error()); len(matches) > 1 && (strings.HasPrefix(err.Error(), "yaml:") || errors.As(err, &yamlTypeError)) {
			line, _ = strconv.Atoi(matches[1])
		}
		out.Valid = false
		out.Errors = append(out.Errors, configValidationError{Line: line, Message: err.Error()})
	}
	return out
}

func decodeConfigInput(w http.ResponseWriter, r *http.Request) (configInput, error) {
	var input configInput
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2*maxConfigBody))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		return input, fmt.Errorf("invalid config request: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return input, fmt.Errorf("request must contain exactly one JSON object")
	}
	return input, nil
}

func (h *Handler) validateConfig(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	if !gateway.IsAdmin(r) {
		writeError(w, http.StatusForbidden, "config validation requires admin role")
		return
	}
	input, err := decodeConfigInput(w, r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, validateConfig(input))
}

func (h *Handler) rawConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if h.configPath == "" {
		writeError(w, http.StatusNotFound, "OMNISWITCH_CONFIG is not set")
		return
	}
	if r.Method == http.MethodGet {
		body, err := os.ReadFile(h.configPath)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "could not read config file")
			return
		}
		format, _ := configFormat(filepath.Ext(h.configPath))
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, map[string]any{"path": h.configPath, "format": format, "body": string(body)})
		return
	}
	if !gateway.IsAdmin(r) {
		writeError(w, http.StatusForbidden, "config writes require admin role")
		return
	}
	input, err := decodeConfigInput(w, r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	fileFormat, _ := configFormat(filepath.Ext(h.configPath))
	if input.Format == "" {
		input.Format = fileFormat
	}
	result := validateConfig(input)
	if !result.Valid {
		writeJSON(w, http.StatusBadRequest, result)
		return
	}
	format, _ := configFormat(input.Format)
	if format != fileFormat {
		writeError(w, http.StatusBadRequest, "format must match the configured file extension")
		return
	}
	if err := writeConfigAtomically(h.configPath, []byte(input.Body)); err != nil {
		writeError(w, http.StatusInternalServerError, "could not save config file")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"saved": true, "path": h.configPath})
}

func writeConfigAtomically(path string, body []byte) error {
	mode := os.FileMode(0o600)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".omniswitch-config-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(file.Name()) }()
	defer func() { _ = file.Close() }()
	if err := file.Chmod(mode); err != nil {
		return err
	}
	if _, err := file.Write(body); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), path)
}
