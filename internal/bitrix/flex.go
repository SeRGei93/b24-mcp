package bitrix

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// FlexString handles JSON values that may be encoded as either a string or a number.
type FlexString string

func (s *FlexString) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		*s = ""
		return nil
	}
	if trimmed[0] == '"' {
		var str string
		if err := json.Unmarshal(trimmed, &str); err != nil {
			return err
		}
		*s = FlexString(str)
		return nil
	}
	var num json.Number
	if err := json.Unmarshal(trimmed, &num); err == nil {
		*s = FlexString(num.String())
		return nil
	}
	return fmt.Errorf("cannot unmarshal %s into FlexString", string(trimmed))
}

// FlexInt handles JSON integers encoded as either numbers or strings.
type FlexInt int

func (i *FlexInt) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		*i = 0
		return nil
	}
	if trimmed[0] == '"' {
		var str string
		if err := json.Unmarshal(trimmed, &str); err != nil {
			return err
		}
		v, err := strconv.Atoi(strings.TrimSpace(str))
		if err != nil {
			return err
		}
		*i = FlexInt(v)
		return nil
	}
	var v int
	if err := json.Unmarshal(trimmed, &v); err != nil {
		return err
	}
	*i = FlexInt(v)
	return nil
}

// FlexScope handles Bitrix24 scope encoded as either a comma-separated string or a JSON string array.
type FlexScope string

func (sc *FlexScope) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		*sc = ""
		return nil
	}
	if trimmed[0] == '[' {
		var arr []string
		if err := json.Unmarshal(trimmed, &arr); err != nil {
			return err
		}
		*sc = FlexScope(strings.Join(arr, ","))
		return nil
	}
	var str string
	if err := json.Unmarshal(trimmed, &str); err != nil {
		return err
	}
	*sc = FlexScope(str)
	return nil
}
