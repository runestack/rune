package format

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/fatih/color"
	"golang.org/x/term"
)

// Error types
const (
	ErrorType   = "error"
	WarningType = "warning"
	InfoType    = "info"
)

// Error colors
var (
	ErrorColor     = color.New(color.FgRed, color.Bold)
	WarningColor   = color.New(color.FgYellow, color.Bold)
	SuccessColor   = color.New(color.FgGreen, color.Bold)
	FileColor      = color.New(color.FgCyan)
	LineColor      = color.New(color.FgHiGreen)
	CodeColor      = color.New(color.FgWhite)
	ContextColor   = color.New(color.FgHiBlack)
	HintColor      = color.New(color.FgYellow, color.Italic)
	HeadingColor   = color.New(color.FgHiWhite, color.Bold)
	HighlightColor = color.New(color.FgHiRed)
)

// ValidationError represents a structured validation error
type ValidationError struct {
	FileName     string `json:"file_name"`
	LineNumber   int    `json:"line_number"`
	Message      string `json:"message"`
	ErrorType    string `json:"error_type"`
	Hint         string `json:"hint,omitempty"`
	AutoFixable  bool   `json:"auto_fixable"`
	FixApplied   bool   `json:"fix_applied"`
	ResourceName string `json:"resource_name,omitempty"`
	ResourceType string `json:"resource_type,omitempty"`
	Context      string `json:"context,omitempty"`
}

// ErrorFormatter is used to format and print error messages
type ErrorFormatter struct {
	FileName      string
	FileData      []byte
	ContextLines  int
	CanAutoFix    bool
	OutputFormat  string
	Errors        []ValidationError
	StartTime     time.Time
	TerminalWidth int
	ErrorCount    int
	WarningCount  int
	FixCount      int
}

// ErrorCategoryMapping maps error patterns to categories
var ErrorCategoryMapping = map[string]string{
	"invalid YAML":               "YAML_SYNTAX",
	"field validation failed":    "FIELD_VALIDATION",
	"missing required field":     "MISSING_FIELD",
	"invalid value":              "INVALID_VALUE",
	"unknown field":              "UNKNOWN_FIELD",
	"duplicate field":            "DUPLICATE_FIELD",
	"unexpected format":          "FORMAT_ERROR",
	"check indentation":          "INDENTATION_ERROR",
	"reference error":            "REFERENCE_ERROR",
	"unrecognized resource type": "UNKNOWN_RESOURCE",
	"must be between":            "RANGE_ERROR",
	"cannot be negative":         "NEGATIVE_VALUE",
	"is required":                "MISSING_REQUIRED",
	"must specify":               "MISSING_SPECIFICATION",
	"must have":                  "MISSING_REQUIREMENT",
}

// Common YAML indentation error patterns
var indentationPatterns = []string{
	"mapping values are not allowed in this context",
	"did not find expected key",
	"unexpected format - check indentation",
	"block sequence entries are not allowed",
	"not enough space",
	"line contains invalid character",
	"invalid leading UTF-8 octet",
}

// Common hint templates
var hintTemplates = map[string]string{
	"YAML_SYNTAX":      "Check your YAML indentation. Each level should be indented with 2 spaces.\n       Make sure the structure follows the correct format for this resource type.",
	"FIELD_VALIDATION": "Review the field value against its expected format or constraints.\n       Common issues include incorrect types, formats, or pattern matching.",
	"MISSING_FIELD":    "Add the required field '%s' to your configuration.\n       This field is mandatory for this resource type.",
	"INVALID_VALUE":    "The value provided is not valid for field '%s'.\n       Check documentation for allowed values and formats.",
	"RANGE_ERROR":      "Values must be within the specified range. Check the min/max requirements.",
	"NEGATIVE_VALUE":   "This field cannot have a negative value. Use zero or a positive number.",
	"MISSING_REQUIRED": "Add the required field to complete the configuration.",
}

// NewErrorFormatter creates a new error formatter
func NewErrorFormatter(filename string, data []byte) *ErrorFormatter {
	width, _, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil {
		width = 80 // Default width if can't detect terminal
	}

	return &ErrorFormatter{
		FileName:      filename,
		FileData:      data,
		ContextLines:  1,
		OutputFormat:  "text",
		StartTime:     time.Now(),
		TerminalWidth: width,
		ErrorCount:    0,
		WarningCount:  0,
		FixCount:      0,
	}
}

// PrintErrorHeader prints a header for errors
func (f *ErrorFormatter) PrintErrorHeader() {
	if f.OutputFormat == "json" {
		return
	}

	// Only print once if we already have errors
	if len(f.Errors) > 0 {
		return
	}

	fmt.Println()
	divider := strings.Repeat("─", f.TerminalWidth)
	ErrorColor.Println("× VALIDATION FAILED", FileColor.Sprintf("%s", f.FileName))
	fmt.Println(divider)
	fmt.Println()
}

// ExtractLineNumber tries to extract a line number from an error message
func (f *ErrorFormatter) ExtractLineNumber(errStr string) int {
	// Try to find line number in common YAML error formats
	lineRegexes := []*regexp.Regexp{
		regexp.MustCompile(`line (\d+)`),
		regexp.MustCompile(`line: (\d+)`),
		regexp.MustCompile(`line:(\d+)`),
	}

	for _, re := range lineRegexes {
		matches := re.FindStringSubmatch(errStr)
		if len(matches) > 1 {
			if num, err := strconv.Atoi(matches[1]); err == nil {
				return num
			}
		}
	}

	return 0
}

// ExtractFieldName tries to extract a field name from an error message
func (f *ErrorFormatter) ExtractFieldName(errStr string) string {
	// Try to extract field name from common patterns
	fieldRegexes := []*regexp.Regexp{
		regexp.MustCompile(`field ([a-zA-Z0-9_.-]+) is`),
		regexp.MustCompile(`field '([a-zA-Z0-9_.-]+)'`),
		regexp.MustCompile(`for field ([a-zA-Z0-9_.-]+)`),
		regexp.MustCompile(`missing required field ([a-zA-Z0-9_.-]+)`),
	}

	for _, re := range fieldRegexes {
		matches := re.FindStringSubmatch(errStr)
		if len(matches) > 1 {
			return matches[1]
		}
	}

	return ""
}

// extractLineContext gets the line and its surrounding context
func (f *ErrorFormatter) extractLineContext(lineNum int) (string, string, string) {
	if f.FileData == nil || lineNum <= 0 {
		return "", "", ""
	}

	scanner := bufio.NewScanner(bytes.NewReader(f.FileData))
	var before []string
	var current string
	var after []string
	lineCount := 0

	// Collect lines around the error
	for scanner.Scan() {
		lineCount++
		if lineCount < lineNum-f.ContextLines {
			continue
		}
		if lineCount == lineNum {
			current = scanner.Text()
		} else if lineCount > lineNum-f.ContextLines && lineCount < lineNum {
			before = append(before, scanner.Text())
		} else if lineCount > lineNum && lineCount <= lineNum+f.ContextLines {
			after = append(after, scanner.Text())
		} else if lineCount > lineNum+f.ContextLines {
			break
		}
	}

	// Format the context
	beforeStr := ""
	if len(before) > 0 {
		beforeStr = strings.Join(before, "\n")
	}

	afterStr := ""
	if len(after) > 0 {
		afterStr = strings.Join(after, "\n")
	}

	return beforeStr, current, afterStr
}

// GenerateHint creates a hint based on the error message
func (f *ErrorFormatter) GenerateHint(errStr, errType string) string {
	// Check if it's an indentation error
	for _, pattern := range indentationPatterns {
		if strings.Contains(errStr, pattern) {
			return hintTemplates["YAML_SYNTAX"]
		}
	}

	// Check if it's a missing field
	if strings.Contains(errStr, "missing required field") || strings.Contains(errStr, "is required") {
		fieldName := f.ExtractFieldName(errStr)
		if fieldName != "" {
			return fmt.Sprintf(hintTemplates["MISSING_FIELD"], fieldName)
		}
		return hintTemplates["MISSING_REQUIRED"]
	}

	// Check if it's a range error
	if strings.Contains(errStr, "must be between") {
		return hintTemplates["RANGE_ERROR"]
	}

	// Check if it's a negative value error
	if strings.Contains(errStr, "negative") {
		return hintTemplates["NEGATIVE_VALUE"]
	}

	// Check if it's an invalid value
	if strings.Contains(errStr, "invalid value") {
		fieldName := f.ExtractFieldName(errStr)
		if fieldName != "" {
			return fmt.Sprintf(hintTemplates["INVALID_VALUE"], fieldName)
		}
		return hintTemplates["FIELD_VALIDATION"]
	}

	// Default hint based on error type
	if hint, ok := hintTemplates[errType]; ok {
		return hint
	}

	return ""
}

// AddError adds a new error to the formatter
func (f *ErrorFormatter) AddError(err ValidationError) {
	f.Errors = append(f.Errors, err)
	if err.ErrorType == ErrorType {
		f.ErrorCount++
	} else if err.ErrorType == WarningType {
		f.WarningCount++
	}
}

// PrintError prints an error with context
func (f *ErrorFormatter) PrintError(errStr string, lineNum int) {
	// Determine error type for categorization
	errCategory := "GENERAL_ERROR"
	for pattern, category := range ErrorCategoryMapping {
		if strings.Contains(strings.ToLower(errStr), strings.ToLower(pattern)) {
			errCategory = category
			break
		}
	}

	// Always use "error" as the error type for counting purposes
	errType := ErrorType

	autoFixable := false
	hint := f.GenerateHint(errStr, errCategory)

	// YAML indentation errors are often auto-fixable
	for _, pattern := range indentationPatterns {
		if strings.Contains(errStr, pattern) {
			if hint == "" {
				hint = "Check YAML indentation. Each level should be indented with 2 spaces."
			}
			autoFixable = true
			break
		}
	}

	if f.OutputFormat == "json" {
		// Get context
		before, current, after := f.extractLineContext(lineNum)
		contextString := ""
		if current != "" {
			if before != "" {
				contextString += before + "\n"
			}
			contextString += current + "\n"
			if after != "" {
				contextString += after
			}
		}

		// Store the error
		f.AddError(ValidationError{
			FileName:    f.FileName,
			LineNumber:  lineNum,
			Message:     errStr,
			ErrorType:   errType,
			Hint:        hint,
			AutoFixable: autoFixable,
			Context:     contextString,
		})

		return
	}

	// Print the error
	indent := "  "
	if lineNum > 0 {
		// Get context
		before, current, after := f.extractLineContext(lineNum)

		// Print line and line number information
		LineColor.Printf("► Line %d:\n", lineNum)

		// Print any before context with line numbers
		if before != "" {
			lines := strings.Split(before, "\n")
			lineStart := lineNum - len(lines)
			for i, line := range lines {
				ContextColor.Printf("%d │ %s\n", lineStart+i, line)
			}
		}

		// Print the error line with highlighting
		CodeColor.Printf("%d │ %s\n", lineNum, current)

		// Print any after context with line numbers
		if after != "" {
			lines := strings.Split(after, "\n")
			for i, line := range lines {
				ContextColor.Printf("%d │ %s\n", lineNum+i+1, line)
			}
		}

		// Print the error message
		fmt.Println()
		ErrorColor.Printf("%sError: %s\n", indent, errStr)

		// Print hint if available
		if hint != "" {
			HintColor.Printf("%sHint: %s\n\n", indent, hint)
		}
	} else {
		ErrorColor.Printf("%sError: %s\n", indent, errStr)
		if hint != "" {
			HintColor.Printf("%sHint: %s\n\n", indent, hint)
		}
	}

	// Always add the error to the error count, regardless of output format
	f.AddError(ValidationError{
		FileName:    f.FileName,
		LineNumber:  lineNum,
		Message:     errStr,
		ErrorType:   errType,
		Hint:        hint,
		AutoFixable: autoFixable,
	})
}

// PrintServiceError prints an error specific to a service
func (f *ErrorFormatter) PrintServiceError(serviceName, errStr string, lineNum int) {
	// Determine error type
	errType := "SERVICE_ERROR"
	for pattern, category := range ErrorCategoryMapping {
		if strings.Contains(strings.ToLower(errStr), strings.ToLower(pattern)) {
			errType = category
			break
		}
	}

	hint := f.GenerateHint(errStr, errType)
	autoFixable := false

	if f.OutputFormat == "json" {
		// Get context
		before, current, after := f.extractLineContext(lineNum)
		contextString := ""
		if current != "" {
			if before != "" {
				contextString += before + "\n"
			}
			contextString += current + "\n"
			if after != "" {
				contextString += after
			}
		}

		// Store the error
		f.AddError(ValidationError{
			FileName:     f.FileName,
			LineNumber:   lineNum,
			Message:      errStr,
			ErrorType:    errType,
			Hint:         hint,
			AutoFixable:  autoFixable,
			ResourceName: serviceName,
			ResourceType: "service",
			Context:      contextString,
		})

		return
	}

	// Print the service error
	indent := "  "

	// Print service name if available
	if serviceName != "" {
		FileColor.Printf("Service '%s':\n", serviceName)
	}

	if lineNum > 0 {
		// Get context
		before, current, after := f.extractLineContext(lineNum)

		// Print line and line number information
		LineColor.Printf("► Line %d:\n", lineNum)

		// Print any before context with line numbers
		if before != "" {
			lines := strings.Split(before, "\n")
			lineStart := lineNum - len(lines)
			for i, line := range lines {
				ContextColor.Printf("%d │ %s\n", lineStart+i, line)
			}
		}

		// Print the error line with highlighting
		CodeColor.Printf("%d │ %s\n", lineNum, current)

		// Print any after context with line numbers
		if after != "" {
			lines := strings.Split(after, "\n")
			for i, line := range lines {
				ContextColor.Printf("%d │ %s\n", lineNum+i+1, line)
			}
		}

		// Print the error message
		fmt.Println()
		ErrorColor.Printf("%sError: %s\n", indent, errStr)

		// Print hint if available
		if hint != "" {
			HintColor.Printf("%sHint: %s\n", indent, hint)
		}
	} else {
		ErrorColor.Printf("%sError: %s\n", indent, errStr)
		if hint != "" {
			HintColor.Printf("%sHint: %s\n", indent, hint)
		}
	}
}

// TryAutoFix attempts to fix common errors
func (f *ErrorFormatter) TryAutoFix(errStr string, lineNum int) (bool, []byte) {
	if f.FileData == nil || lineNum <= 0 || !f.CanAutoFix {
		return false, nil
	}

	// Clone the file data
	newData := make([]byte, len(f.FileData))
	copy(newData, f.FileData)

	// Check if it's an indentation error
	if isIndentationError(errStr) {
		return f.fixIndentation(lineNum, newData)
	}

	// Check if it's a boolean format issue
	if strings.Contains(errStr, "cannot unmarshal") &&
		(strings.Contains(errStr, "bool") || strings.Contains(errStr, "boolean")) {
		return f.fixBooleanValue(lineNum, newData)
	}

	// Check if it's a required field issue
	if strings.Contains(errStr, "missing required field") {
		return f.fixMissingField(errStr, lineNum, newData)
	}

	return false, nil
}

// isIndentationError checks if an error is related to indentation
func isIndentationError(errStr string) bool {
	for _, pattern := range indentationPatterns {
		if strings.Contains(errStr, pattern) {
			return true
		}
	}
	return false
}

// fixIndentation attempts to fix indentation issues
func (f *ErrorFormatter) fixIndentation(lineNum int, data []byte) (bool, []byte) {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	var lines []string
	lineCount := 0

	prevIndentLevel := 0

	// Read all lines
	for scanner.Scan() {
		lineCount++
		line := scanner.Text()

		// Only calculate indentation if not on the error line
		if lineCount != lineNum {
			// Get the indentation level of non-error lines
			indentMatch := regexp.MustCompile(`^(\s*)`).FindString(line)
			prevIndentLevel = len(indentMatch) / 2
		}

		// If this is the error line, try to fix indentation
		if lineCount == lineNum {
			// Get the previous non-empty line's indentation
			for i := len(lines) - 1; i >= 0; i-- {
				if strings.TrimSpace(lines[i]) != "" {
					prevIndent := regexp.MustCompile(`^(\s*)`).FindString(lines[i])
					prevIndentLevel = len(prevIndent) / 2
					break
				}
			}

			// Check if this line starts with a key
			if strings.Contains(line, ":") {
				keyMatch := regexp.MustCompile(`^(\s*)(\S+):`).FindStringSubmatch(line)
				if len(keyMatch) >= 3 {
					// This is a key, should it be indented?
					if strings.HasSuffix(strings.TrimSpace(lines[len(lines)-1]), ":") {
						// Previous line ends with :, this should be indented more
						correctIndent := strings.Repeat("  ", prevIndentLevel+1)
						correctedLine := correctIndent + strings.TrimLeft(line, " \t")
						lines = append(lines, correctedLine)
						continue
					} else if strings.Contains(lines[len(lines)-1], ":") &&
						!strings.HasSuffix(strings.TrimSpace(lines[len(lines)-1]), ":") {
						// Previous line has a key: value, this should have the same indentation
						correctIndent := strings.Repeat("  ", prevIndentLevel)
						correctedLine := correctIndent + strings.TrimLeft(line, " \t")
						lines = append(lines, correctedLine)
						continue
					}
				}
			}

			// Check if it's a list item
			if strings.TrimLeft(line, " \t")[0] == '-' {
				// Is the previous line also a list item?
				if strings.Contains(strings.TrimSpace(lines[len(lines)-1]), "- ") {
					// Should have same indentation as previous list item
					correctIndent := regexp.MustCompile(`^(\s*)`).FindString(lines[len(lines)-1])
					correctedLine := correctIndent + strings.TrimLeft(line, " \t")
					lines = append(lines, correctedLine)
					continue
				} else if strings.HasSuffix(strings.TrimSpace(lines[len(lines)-1]), ":") {
					// Previous line ends with :, this should be a list under it
					correctIndent := strings.Repeat("  ", prevIndentLevel+1)
					correctedLine := correctIndent + strings.TrimLeft(line, " \t")
					lines = append(lines, correctedLine)
					continue
				}
			}

			// If we're here, we couldn't determine a specific fix, try basic approach
			// Based on colon patterns
			if strings.HasSuffix(strings.TrimSpace(lines[len(lines)-1]), ":") {
				// Previous line ends with :, this should be indented more
				correctIndent := strings.Repeat("  ", prevIndentLevel+1)
				correctedLine := correctIndent + strings.TrimLeft(line, " \t")
				lines = append(lines, correctedLine)
			} else {
				// Just add the line as-is for now
				lines = append(lines, line)
			}
		} else {
			// Not the error line, just add it
			lines = append(lines, line)
		}
	}

	// Create new data
	newData := []byte(strings.Join(lines, "\n"))
	if len(f.FileData) > 0 && f.FileData[len(f.FileData)-1] == '\n' {
		// Preserve trailing newline if original had one
		newData = append(newData, '\n')
	}

	// Only return true if we actually made changes
	return !bytes.Equal(data, newData), newData
}

// fixBooleanValue attempts to fix boolean values
func (f *ErrorFormatter) fixBooleanValue(lineNum int, data []byte) (bool, []byte) {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	var lines []string
	lineCount := 0

	// Read all lines
	for scanner.Scan() {
		lineCount++
		line := scanner.Text()

		// If this is the error line, try to fix boolean value
		if lineCount == lineNum {
			// Find patterns like key: yes, key: no, key: True, key: False
			boolPattern := regexp.MustCompile(`^(\s*)([^:]+):\s*(yes|no|Yes|No|TRUE|FALSE|True|False)(.*)$`)
			matches := boolPattern.FindStringSubmatch(line)

			if len(matches) >= 4 {
				indent := matches[1]
				key := matches[2]
				value := matches[3]
				rest := matches[4]

				// Map to correct boolean values
				correctValue := "false"
				if strings.ToLower(value) == "yes" || strings.ToLower(value) == "true" {
					correctValue = "true"
				}

				// Create corrected line
				correctedLine := fmt.Sprintf("%s%s: %s%s", indent, key, correctValue, rest)
				lines = append(lines, correctedLine)
				continue
			}
		}

		// Not the error line or not a boolean error, just add it
		lines = append(lines, line)
	}

	// Create new data
	newData := []byte(strings.Join(lines, "\n"))
	if len(f.FileData) > 0 && f.FileData[len(f.FileData)-1] == '\n' {
		// Preserve trailing newline if original had one
		newData = append(newData, '\n')
	}

	// Only return true if we actually made changes
	return !bytes.Equal(data, newData), newData
}

// fixMissingField attempts to add missing required fields
func (f *ErrorFormatter) fixMissingField(errStr string, lineNum int, data []byte) (bool, []byte) {
	fieldName := f.ExtractFieldName(errStr)
	if fieldName == "" {
		return false, nil
	}

	scanner := bufio.NewScanner(bytes.NewReader(data))
	var lines []string
	lineCount := 0

	// Find the parent object level for this field
	parentIndent := ""
	sectionFound := false

	// First pass to determine context
	for scanner.Scan() {
		line := scanner.Text()
		lineCount++

		// If we're at or past the error line
		if lineCount >= lineNum {
			// Look for a parent section that might be missing a field
			indentMatch := regexp.MustCompile(`^(\s*)`).FindString(line)
			if parentIndent == "" || len(indentMatch) < len(parentIndent) {
				parentIndent = indentMatch
				sectionFound = true
			}
			break
		}
	}

	// Reset for second pass
	lineCount = 0
	scanner = bufio.NewScanner(bytes.NewReader(data))
	lines = []string{}

	// Second pass to insert missing field
	for scanner.Scan() {
		line := scanner.Text()
		lineCount++

		lines = append(lines, line)

		// If we've found a section and we're at the error line
		if sectionFound && lineCount == lineNum {
			// Insert the missing field with appropriate default value
			var defaultValue string

			// Choose appropriate default based on field name
			switch strings.ToLower(fieldName) {
			case "name":
				defaultValue = "default-name"
			case "image":
				defaultValue = "default-image:latest"
			case "port":
				defaultValue = "80"
			case "scale":
				defaultValue = "1"
			case "namespace":
				defaultValue = "default"
			default:
				defaultValue = "" // Empty string for unknown fields
			}

			// Create field line with proper indentation
			fieldLine := fmt.Sprintf("%s  %s: %s", parentIndent, fieldName, defaultValue)
			lines = append(lines, fieldLine)
		}
	}

	// Create new data
	newData := []byte(strings.Join(lines, "\n"))
	if len(f.FileData) > 0 && f.FileData[len(f.FileData)-1] == '\n' {
		// Preserve trailing newline if original had one
		newData = append(newData, '\n')
	}

	// Only return true if we actually made changes
	return !bytes.Equal(data, newData), newData
}

// FormatAsJSON formats all errors as a JSON string
func (f *ErrorFormatter) FormatAsJSON() string {
	result := map[string]interface{}{
		"filename":      f.FileName,
		"errors":        f.Errors,
		"error_count":   f.ErrorCount,
		"warning_count": f.WarningCount,
		"fix_count":     f.FixCount,
		"time":          time.Since(f.StartTime).Seconds(),
		"success":       f.ErrorCount == 0,
	}

	jsonBytes, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return fmt.Sprintf(`{"error":"Failed to marshal JSON: %s"}`, err.Error())
	}

	return string(jsonBytes)
}

// PrintErrorSummary prints a summary of all errors categorized by type
func (f *ErrorFormatter) PrintErrorSummary() {
	if f.OutputFormat == "json" || len(f.Errors) == 0 {
		return
	}

	// Group errors by type
	byType := make(map[string][]ValidationError)
	for _, err := range f.Errors {
		byType[err.ErrorType] = append(byType[err.ErrorType], err)
	}

	// Get sorted list of error types
	var errorTypes []string
	for errType := range byType {
		errorTypes = append(errorTypes, errType)
	}
	sort.Strings(errorTypes)

	// Print the summary
	fmt.Println()
	HeadingColor.Printf("Error summary for %s:\n", f.FileName)
	for _, errType := range errorTypes {
		errors := byType[errType]
		fmt.Printf("  %s: %d errors\n", errType, len(errors))
	}

	// Print auto-fix summary if any fixes were applied
	if f.FixCount > 0 {
		SuccessColor.Printf("  Auto-fixed: %d issues\n", f.FixCount)
	}

	fmt.Println()
}

// PrintSuccess prints a success message
func PrintSuccess(message string) {
	SuccessColor.Println(message)
}

// PrintLintSummary prints a summary of the linting process
func PrintLintSummary(fileCount, errorCount, fixCount int, duration time.Duration) {
	fmt.Println()
	HeadingColor.Println("Lint summary:")
	fmt.Printf("  Files:     %d\n", fileCount)
	fmt.Printf("  Errors:    %d\n", errorCount)
	if fixCount > 0 {
		SuccessColor.Printf("  Auto-fixed: %d\n", fixCount)
	}
	fmt.Printf("  Time:      %.2fs\n", duration.Seconds())
	fmt.Println()

	if errorCount == 0 {
		SuccessColor.Println("✓ All files passed validation!")
	} else {
		ErrorColor.Printf("✗ Found %d validation errors\n", errorCount)
	}
}
