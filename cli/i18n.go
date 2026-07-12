package cli

import "strings"

var currentLang = "id"

// setLanguage sets the active language for the CLI TUI.
func setLanguage(lang string) {
	lang = strings.ToLower(strings.TrimSpace(lang))
	if lang == "en" || lang == "english" {
		currentLang = "en"
	} else {
		currentLang = "id"
	}
}

// T returns either the Indonesian or English translation of a string based on the active language.
func T(idText, enText string) string {
	if currentLang == "en" {
		return enText
	}
	return idText
}
