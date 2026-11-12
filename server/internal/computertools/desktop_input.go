package computertools

import (
	"encoding/json"
	"io"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"io.yueyang/tom/internal/computer"
)

// desktopFields 拒绝重复键、大小写别名、空值和尾随对象；各动作随后只允许自己的字段。
func desktopFields(arguments string) (map[string]json.RawMessage, bool) {
	if len(arguments) > 24<<10 || !utf8.ValidString(arguments) {
		return nil, false
	}
	decoder := json.NewDecoder(strings.NewReader(arguments))
	first, err := decoder.Token()
	if err != nil || first != json.Delim('{') {
		return nil, false
	}
	fields := map[string]json.RawMessage{}
	for decoder.More() {
		field, err := decoder.Token()
		name, ok := field.(string)
		if err != nil || !ok || fields[name] != nil || len(fields) >= 12 {
			return nil, false
		}
		var raw json.RawMessage
		if decoder.Decode(&raw) != nil || string(raw) == "null" {
			return nil, false
		}
		fields[name] = raw
	}
	last, err := decoder.Token()
	return fields, err == nil && last == json.Delim('}') && decoder.Decode(new(any)) == io.EOF
}

func desktopInput(session computer.ExecutionSession, sequence int64, fields map[string]json.RawMessage) (computer.DesktopInput, bool) {
	result := computer.DesktopInput{Session: session, Sequence: sequence}
	var action string
	if json.Unmarshal(fields["action"], &action) != nil {
		return result, false
	}
	allowed := map[string]bool{"action": true}
	for _, name := range map[string][]string{"click": {"x", "y", "button"}, "double_click": {"x", "y", "button"}, "move": {"x", "y"}, "drag": {"x", "y", "to_x", "to_y", "button"}, "scroll": {"x", "y", "direction", "amount"}, "type": {"text"}, "key": {"keys"}}[action] {
		allowed[name] = true
	}
	if len(allowed) == 1 {
		return result, false
	}
	for name := range fields {
		if !allowed[name] {
			return result, false
		}
	}
	readInt := func(name string, maximum int) (int, bool) {
		var value int
		err := json.Unmarshal(fields[name], &value)
		return value, err == nil && value >= 0 && value < maximum
	}
	pointer := func(x, y int, buttons uint8) {
		result.Events = append(result.Events, computer.DesktopEvent{Kind: "pointer", X: x, Y: y, Buttons: buttons})
	}
	keyEvent := func(key uint32, down bool) {
		result.Events = append(result.Events, computer.DesktopEvent{Kind: "key", Key: key, Down: down})
	}
	switch action {
	case "type":
		var text string
		if json.Unmarshal(fields["text"], &text) != nil || utf8.RuneCountInString(text) < 1 || utf8.RuneCountInString(text) > 256 {
			return result, false
		}
		for _, character := range text {
			key, ok := desktopCharacter(character)
			if !ok {
				return result, false
			}
			keyEvent(key, true)
			keyEvent(key, false)
		}
	case "key":
		var names []string
		if json.Unmarshal(fields["keys"], &names) != nil || len(names) < 1 || len(names) > 8 {
			return result, false
		}
		keys := make([]uint32, 0, len(names))
		for _, name := range names {
			key, ok := desktopKey(name)
			if !ok {
				return result, false
			}
			keys = append(keys, key)
			keyEvent(key, true)
		}
		for index := len(keys) - 1; index >= 0; index-- {
			keyEvent(keys[index], false)
		}
	default:
		x, validX := readInt("x", computer.DesktopWidth)
		y, validY := readInt("y", computer.DesktopHeight)
		if !validX || !validY {
			return result, false
		}
		var button uint8 = 1
		if raw, exists := fields["button"]; exists {
			var name string
			if json.Unmarshal(raw, &name) != nil {
				return result, false
			}
			button = map[string]uint8{"left": 1, "middle": 2, "right": 4}[name]
			if button == 0 {
				return result, false
			}
		}
		pointer(x, y, 0)
		switch action {
		case "click", "double_click":
			pointer(x, y, button)
			pointer(x, y, 0)
			if action == "double_click" {
				pointer(x, y, button)
				pointer(x, y, 0)
			}
		case "drag":
			toX, validX := readInt("to_x", computer.DesktopWidth)
			toY, validY := readInt("to_y", computer.DesktopHeight)
			if !validX || !validY {
				return result, false
			}
			pointer(x, y, button)
			pointer(toX, toY, button)
			pointer(toX, toY, 0)
		case "scroll":
			var direction string
			amount, valid := readInt("amount", 21)
			if !valid || amount < 1 || json.Unmarshal(fields["direction"], &direction) != nil {
				return result, false
			}
			button = map[string]uint8{"up": 8, "down": 16, "left": 32, "right": 64}[direction]
			if button == 0 {
				return result, false
			}
			for range amount {
				pointer(x, y, button)
				pointer(x, y, 0)
			}
		}
	}
	return result, result.Validate() == nil
}

func desktopCharacter(character rune) (uint32, bool) {
	switch character {
	case '\n', '\r':
		return 0xff0d, true
	case '\t':
		return 0xff09, true
	}
	if unicode.IsControl(character) || character == utf8.RuneError {
		return 0, false
	}
	key := uint32(character)
	if key > 255 {
		key |= 0x01000000
	}
	return key, true
}

func desktopKey(name string) (uint32, bool) {
	keys := map[string]uint32{"SHIFT": 0xffe1, "CTRL": 0xffe3, "ALT": 0xffe9, "SUPER": 0xffeb, "ENTER": 0xff0d, "TAB": 0xff09, "ESC": 0xff1b, "BACKSPACE": 0xff08, "DELETE": 0xffff, "HOME": 0xff50, "END": 0xff57, "PAGEUP": 0xff55, "PAGEDOWN": 0xff56, "LEFT": 0xff51, "UP": 0xff52, "RIGHT": 0xff53, "DOWN": 0xff54}
	if key, ok := keys[name]; ok {
		return key, true
	}
	if strings.HasPrefix(name, "F") {
		number, err := strconv.Atoi(strings.TrimPrefix(name, "F"))
		if err == nil && number >= 1 && number <= 12 && name == "F"+strconv.Itoa(number) {
			return uint32(0xffbd + number), true
		}
	}
	if utf8.RuneCountInString(name) != 1 {
		return 0, false
	}
	character, _ := utf8.DecodeRuneInString(name)
	return desktopCharacter(character)
}
