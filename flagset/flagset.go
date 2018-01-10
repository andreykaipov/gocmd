/*
 * gocmd
 * For the full copyright and license information, please view the LICENSE.txt file.
 */

// Package flagset provides functions for handling command line arguments
package flagset

import (
	"errors"
	"fmt"
	"os"
	"reflect"
	"regexp"
	"strconv"
	"strings"
)

// Options represents the options that can be set when creating a new flag set
type Options struct {
	// Flags represent the user defined command line arguments and commands.
	// When it's a struct type, each field represent an argument or command.
	Flags interface{}
	// Args hold command line arguments. Default is os.Args
	Args []string
}

// New returns a flag set by the given options
func New(options Options) (*FlagSet, error) {
	// Check the options
	if options.Flags == nil {
		return nil, fmt.Errorf("flags are required")
	} else if !strings.HasPrefix(fmt.Sprintf("%T", options.Flags), "*struct") {
		return nil, fmt.Errorf("flags must be a struct pointer")
	}

	if options.Args == nil {
		options.Args = os.Args // default
	}

	// Init vars
	flagSet := FlagSet{
		flagsRaw: options.Flags,
		argsRaw:  make([]string, len(options.Args)),
	}
	copy(flagSet.argsRaw, options.Args) // take a copy

	// Parse flags and args
	if flagSet.flagsRaw != nil {
		var errs []error
		flagSet.flags, errs = structToFlags(flagSet.flagsRaw)
		if errs != nil {
			return nil, errs[0] // return the first error
		}
	}
	flagSet.parseArgs()

	// Iterate over the flags and apply values to the fields
	for _, flag := range flagSet.flags {
		// Only argument fields can have values
		if flag.kind != "arg" {
			continue
		}

		// Iterate over the args (last argument wins)
		for _, arg := range flag.args {
			// Only arguments (skip commands and argument values)
			if arg.kind != "arg" {
				continue
			}
			flag.valueBy = "arg" // prevent default and env values to override it

			// Handle truthy bool arguments (i.e. `-b --bool`. But not `-b=`)
			if (flag.valueType == "bool" || flag.valueType == "[]bool") && arg.value == "" && !arg.unset {
				arg.value = "true"
			}

			// Handle empty values
			if arg.value == "" {
				if ((flag.valueType == "bool" || flag.valueType == "[]bool") && arg.unset) || ((flag.valueType == "string" || flag.valueType == "[]string") && !arg.unset) {
					// For example: `--bool=`, `--string`
					arg.err = fmt.Errorf("argument %s%s needs a value", arg.dash, arg.name)
				} else if flag.valueType != "bool" && flag.valueType != "[]bool" && flag.valueType != "string" && flag.valueType != "[]string" {
					// For example: `--int`
					arg.err = fmt.Errorf("argument %s%s needs a value", arg.dash, arg.name)
				}
			}

			// Do not continue if the argument has an error
			if arg.err != nil {
				continue
			}

			// Update the flag value
			if flag.delimiter != "" && strings.HasPrefix(flag.valueType, "[]") {
				values := strings.Split(arg.value, flag.delimiter)
				for _, v := range values {
					// Ignore empty ones
					v = strings.TrimSpace(v)
					if v == "" {
						continue
					}
					if err := flagSet.setFlag(flag.id, v); err != nil {
						arg.err = err
					}
				}
			} else {
				if err := flagSet.setFlag(flag.id, arg.value); err != nil {
					arg.err = err
				}
			}
		}
	}

	// Iterate over the flags and update their values
	for _, flag := range flagSet.flags {
		if flag.valueBy == "arg" {
			// Check errors
			for _, arg := range flag.args {
				// If there is an argument error then
				if arg.err != nil {
					// Unset the flag value
					err := flagSet.unsetFlag(flag.id)
					if err != nil {
						flag.err = err
						continue
					}
				}
			}
			continue
		} else if ev, ok := os.LookupEnv(flag.env); ok {
			flag.valueBy = "env"
			if err := flagSet.setFlag(flag.id, ev); err != nil {
				flag.err = err
				continue
			}
		} else if flag.valueDefault != "" {
			flag.valueBy = "default"
			if err := flagSet.setFlag(flag.id, flag.valueDefault); err != nil {
				flag.err = err
				continue
			}
		}
	}

	// Iterate over the flags and check the required arguments
	for _, flag := range flagSet.flags {
		if !flag.required {
			continue // skip non required flags
		} else if flag.args != nil {
			continue // skip if it has arguments
		}

		if flag.kind == "command" {
			flag.err = fmt.Errorf("command %s is required", flag.command)
			continue
		} else if flag.kind == "arg" {
			// Check the parent flag
			command := ""
			if flag.parentIndex != nil {
				parentFlag := flagSet.lookupFlagByIndex(flag.parentIndex)
				if parentFlag != nil {
					command = parentFlag.command
				}
				// If the parent flag (command) has no argument then
				if parentFlag != nil && parentFlag.args == nil {
					// It's not in the argument list so the flag isn't required
					continue
				}
			}

			// Prepare error
			arg := ""
			if flag.short != "" {
				arg = fmt.Sprintf("%s%s", "-", flag.short)
			} else if flag.long != "" {
				arg = fmt.Sprintf("%s%s", "--", flag.long)
			}
			eMsg := fmt.Sprintf("argument %s is required", arg)
			if command != "" {
				eMsg = fmt.Sprintf("%s for %s command", eMsg, command)
			}
			flag.err = errors.New(eMsg)
			continue
		}
	}

	return &flagSet, nil
}

// FlagSet represents a flag set
type FlagSet struct {
	flags          []*Flag
	flagsRaw       interface{}
	args           []*Arg
	argsRaw        []string
	argsParsed     bool
	commands       []*Command
	commandsParsed bool
}

// FlagByName returns a flag by the given name or returns nil if it doesn't exist
// Nested commands are separated by dot (i.e. Foo.Bar)
func (flagSet *FlagSet) FlagByName(name string) *Flag {
	if name == "" {
		return nil
	}

	// Init vars
	var result *Flag
	names := strings.Split(name, ".")
	flags := flagSet.flags

	// Iterate over the names and find the flag
	curParentID := -1
	for _, name := range names {
		found := false
		for _, flag := range flags {
			if flag.parentID != curParentID || flag.name != name {
				continue
			}
			result = flag
			curParentID = flag.id
			found = true
			break
		}
		if !found {
			return nil
		}
	}

	return result
}

// FlagArgs returns the flag arguments those exist in the argument list
// If the flag is an argument then it return it's values (i.e. [foo bar] for `-f=foo -f=bar`)
// If it's a command then it returns the command name and the rest of the arguments (i.e. [command -f=true --bar=baz qux] for `command -f --bar=baz qux`).
// Nested commands are separated by dot (i.e. Foo.Bar)
func (flagSet *FlagSet) FlagArgs(name string) []string {
	if name == "" {
		return nil
	}

	// Init vars
	var result []string

	flag := flagSet.FlagByName(name)
	if flag == nil || flag.args == nil {
		return nil
	}

	// Iterate over the arguments
	for _, v := range flag.args {
		if flag.kind == "arg" {
			result = append(result, v.value)
		} else if flag.kind == "command" {
			arg := ""
			if v.kind == "arg" {
				arg = v.dash
				if v.name != "" {
					arg = fmt.Sprintf("%s%s", arg, v.name)
				}
				if v.value != "" {
					arg = fmt.Sprintf("%s=%s", arg, v.value)
				}
			} else {
				arg = v.name
			}
			result = append(result, arg)
		}
	}

	return result
}

// Flags returns the flags
func (flagSet *FlagSet) Flags() []*Flag {
	return flagSet.flags
}

// Errors returns the flag and argument errors
func (flagSet *FlagSet) Errors() []error {
	var result []error
	for _, flag := range flagSet.flags {
		if flag.err != nil {
			result = append(result, flag.err)
		}
		for _, arg := range flag.args {
			if arg != nil && arg.err != nil {
				result = append(result, arg.err)
			}
		}
	}
	return result
}

// lookupFlagByID returns a flag by the given id or returns nil if it doesn't exist
func (flagSet *FlagSet) lookupFlagByID(id int) *Flag {
	if id < 0 {
		return nil
	}
	for _, v := range flagSet.flags {
		if v.id == id {
			return v
		}
	}
	return nil
}

// lookupFlagByIndex returns a flag by the given field index or returns nil if it doesn't exist
func (flagSet *FlagSet) lookupFlagByIndex(index []int) *Flag {
	if index == nil {
		return nil
	}
	for _, v := range flagSet.flags {
		if fmt.Sprint(v.fieldIndex) == fmt.Sprint(index) { // faster then reflect.DeepEqual
			return v
		}
	}
	return nil
}

// parseCommands parses the raw arguments and updates the commands
func (flagSet *FlagSet) parseCommands() {
	// Init vars
	flagSet.commands = make([]*Command, 0) // reset

	// Commands are defined by flags so iterate over the flags and update commands
	lookup := map[int]int{}
	cnt := 0
	for _, flag := range flagSet.flags {
		if flag.kind == "command" {
			// Init the command
			newCmd := Command{
				id:        cnt,
				command:   flag.command,
				flagID:    flag.id,
				parentID:  -1,
				argID:     -1,
				indexFrom: -1,
				indexTo:   -1,
			}
			lookup[flag.id] = cnt // for command id by flag id

			if flag.parentIndex != nil {
				parentFlag := flagSet.lookupFlagByIndex(flag.parentIndex)
				if parentFlag != nil {
					if pid, ok := lookup[parentFlag.id]; ok {
						newCmd.parentID = pid // it must exist since nested commands come after parent commands
					}
				}
			}
			flagSet.commands = append(flagSet.commands, &newCmd)
			cnt++
		}
	}

	// Iterate over the raw arguments and update commands
	lenCmds := len(flagSet.commands)
	for argIndex, argVal := range flagSet.argsRaw {
		for i := 0; i < lenCmds; i++ {
			cmd := flagSet.commands[i]
			// Checking argID prevents issues when a nested command has same name as parent command (i.e. `app foo -b foo -b`)
			if cmd.argID == -1 && cmd.command == argVal {
				found := false
				// If it's a nested command then
				if cmd.parentID != -1 {
					// Make sure it's after the parent command
					for j := 0; j < lenCmds; j++ {
						parentArgID := flagSet.commands[j].argID
						if parentArgID != -1 && parentArgID < argIndex {
							found = true
							break
						}
					}
				} else {
					found = true
				}

				if found == true {
					cmd.indexFrom = argIndex
					cmd.argID = argIndex
					cmd.updatedBy = append(cmd.updatedBy, "found in the arguments")
					// If the previous command is found in the arguments then
					if i > 0 && flagSet.commands[i-1].argID != -1 {
						// Update the previous command
						prevCmd := flagSet.commands[i-1]
						prevCmd.indexTo = argIndex
						prevCmd.updatedBy = append(prevCmd.updatedBy, "previously found in the arguments")
					}
					break
				}
			}
		}
	}

	// Update indexTo value (i.e. commands: foo, bar `app foo -b`. indexTo for foo must be 2)
	for i := 0; i < lenCmds; i++ {
		cmd := flagSet.commands[i]
		// If the command is not found or indexTo is already up to date then
		if cmd.argID == -1 || cmd.indexTo != -1 {
			continue
		}

		// If it's the last loop then
		if i+1 == lenCmds {
			cmd.indexTo = len(flagSet.argsRaw)
			cmd.updatedBy = append(cmd.updatedBy, "last loop")
			continue
		}

		// Otherwise search for the following command
		for j := i + 1; j < lenCmds; j++ {
			if flagSet.commands[j].indexFrom != -1 {
				cmd.indexTo = flagSet.commands[j].indexFrom
				cmd.updatedBy = append(cmd.updatedBy, "next command")
				break
			}
		}
		// If it's not found then
		if cmd.indexTo == -1 {
			cmd.indexTo = len(flagSet.argsRaw)
			cmd.updatedBy = append(cmd.updatedBy, "last command")
		}
	}

	// Iterate over the commands and update flags
	for _, cmd := range flagSet.commands {
		for _, flag := range flagSet.flags {
			if cmd.flagID == flag.id {
				flag.commandID = cmd.id
				break
			}
		}
	}

	flagSet.commandsParsed = true
}

// parseArgs parses the raw arguments and updates the arguments
func (flagSet *FlagSet) parseArgs() {
	// Commands must be parsed before arguments
	if !flagSet.commandsParsed {
		flagSet.parseCommands()
	} else if flagSet.argsParsed {
		// Do not parse second time
		return
	}

	// Init vars
	flagSet.args = make([]*Arg, 0) // reset

	// Iterate over the raw arguments and create the default arguments
	for argIndex, argVal := range flagSet.argsRaw {
		// Init the new argument
		newArg := Arg{
			id:        argIndex,
			arg:       argVal,
			flagID:    -1,
			commandID: -1,
			parentID:  -1,
			indexFrom: argIndex,
			indexTo:   argIndex + 1,
		}

		// Check commands
		for _, cmd := range flagSet.commands {
			if argIndex == cmd.argID {
				newArg.name = newArg.arg
				newArg.kind = "command"
				newArg.flagID = cmd.flagID
				newArg.commandID = cmd.id
				newArg.indexFrom = cmd.indexFrom
				newArg.indexTo = cmd.indexTo
				newArg.updatedBy = append(newArg.updatedBy, "command argID matched argIndex")
				break
			} else {
				if cmd.indexFrom < newArg.indexFrom && cmd.indexTo >= newArg.indexTo {
					newArg.commandID = cmd.id
					newArg.updatedBy = append(newArg.updatedBy, "in command range")
					break
				}
			}
		}

		if newArg.kind == "" {
			newArg.kind = "arg"
		}

		flagSet.args = append(flagSet.args, &newArg)
	}

	// Iterate over the arguments and update
	argsLen := len(flagSet.args)
	for argIndex, arg := range flagSet.args {
		if arg.kind != "arg" {
			continue
		}

		arg.name = strings.TrimSpace(strings.TrimLeft(arg.arg, "-"))

		if strings.HasPrefix(arg.arg, "--") {
			arg.dash = "--"
		} else if strings.HasPrefix(arg.arg, "-") {
			arg.dash = "-"
		}
		// Unnamed argument
		if arg.dash == "" {
			arg.unnamed = true
			continue
		}

		// Check equal character for the value (i.e. `--arg=value`)
		ieq := strings.Index(arg.name, "=")
		iqo := strings.Index(arg.name, "\"")
		if iqo == -1 {
			iqo = strings.Index(arg.name, "'")
		}
		if ieq > -1 && (ieq < iqo || iqo == -1) { // avoids `"a=" 'a='`
			arg.hasEq = true
			s := strings.SplitN(arg.name, "=", 2)
			arg.name = s[0]
			arg.value = strings.Join(s[1:], "")
			if strings.HasPrefix(arg.value, "\"") {
				arg.value = strings.Trim(arg.value, "\"")
			} else if strings.HasPrefix(arg.value, "'") {
				arg.value = strings.Trim(arg.value, "'")
			}
		} else {
			// Check the next argument (i.e. `[--arg value]`)
			if argIndex+1 < argsLen {
				nextArg := flagSet.args[argIndex+1]
				if nextArg.kind == "arg" && !strings.HasPrefix(nextArg.arg, "-") {
					arg.value = nextArg.arg
					arg.indexTo = nextArg.indexTo
					if strings.HasPrefix(arg.value, "\"") {
						arg.value = strings.Trim(arg.value, "\"")
					} else if strings.HasPrefix(arg.value, "'") {
						arg.value = strings.Trim(arg.value, "'")
					}
					nextArg.kind = "argval"
					nextArg.value = arg.value
					nextArg.parentID = arg.id
				}
			}
		}

		if arg.hasEq && arg.value == "" {
			arg.unset = true // for example `--arg= --arg="" --arg=''`
		}
	}

	// Iterate over the flags and update the values
	for _, flag := range flagSet.flags {

		// Commands
		if flag.kind == "command" {
			// Iterate over the command arguments and add into the flag arguments
			for _, cmd := range flagSet.commands {
				// If the command is found then
				if cmd.argID != -1 && cmd.flagID == flag.id {
					for _, arg := range flagSet.args {
						if arg.commandID == cmd.id {
							flag.args = append(flag.args, arg)
						}
					}
					break
				}
			}
			continue // skip rest of the code
		}

		// Args
		if flag.kind == "arg" {

			// If the flag has parent then
			if flag.parentID != -1 {
				// Make sure the argument comes after the parent comand and before another command (i.e. `app command1 --foo command2 --foo`)
				parentFlag := flagSet.lookupFlagByIndex(flag.parentIndex)
				if parentFlag != nil && parentFlag.args != nil {
					// Iterate over the parent flag's arguments
					for _, pArg := range parentFlag.args {
						if pArg.name != "" && (flag.short == pArg.name || flag.long == pArg.name) {
							flag.updatedBy = append(flag.updatedBy, "matched argument")
							flag.commandID = pArg.commandID
							pArg.flagID = flag.id
							pArg.updatedBy = append(pArg.updatedBy, "matched flag")
							flag.args = append(flag.args, pArg)
						}
						// Don't break here for getting the last argument value (i.e. `-f=true -f=false`)
					}
				}
			} else {
				// Iterate over the arguments
				for _, arg := range flagSet.args {
					// Flag has no parent so make sure the argument is not belong to any other command (i.e. `app command --foo`)
					// Command arguments are handled previously
					if arg.commandID == -1 && arg.name != "" && (flag.short == arg.name || flag.long == arg.name) {
						flag.updatedBy = append(flag.updatedBy, "top level flag")
						arg.updatedBy = append(arg.updatedBy, "top level arg")
						arg.flagID = flag.id
						flag.args = append(flag.args, arg)
					}
					// Don't break here for getting the last argument value (i.e. `-f=true -f=false`)
				}
			}
		}
	}

	flagSet.argsParsed = true
}

// setFlag sets a flag value by the given flag id and value
func (flagSet *FlagSet) setFlag(id int, value string) error {
	if id < 0 {
		return errors.New("flag id is required")
	}

	// Check the flag
	flag := flagSet.lookupFlagByID(id)
	if flag == nil {
		return fmt.Errorf("no flag for id %d", id)
	}
	rValue := reflect.ValueOf(flagSet.flagsRaw).Elem()
	fv := rValue.FieldByIndex(flag.fieldIndex)
	if !fv.CanSet() {
		return fmt.Errorf("flag %s can't be set", flag.name)
	}

	// Set the value
	switch flag.valueType {
	case "bool":
		if value != "true" && value != "false" {
			return fmt.Errorf("failed to parse '%s' as bool", value)
		}
		if value == "true" {
			fv.SetBool(true)
		} else if value == "false" {
			fv.SetBool(false)
		}
	case "float64":
		if value != "" {
			v, err := strconv.ParseFloat(value, 64)
			if err != nil {
				return fmt.Errorf("failed to parse '%s' as float64", value)
			}
			fv.SetFloat(v)
		}
	case "int":
		if value != "" {
			v, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				return fmt.Errorf("failed to parse '%s' as int", value)
			}
			fv.SetInt(v)
		}
	case "int64":
		if value != "" {
			v, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				return fmt.Errorf("failed to parse '%s' as int64", value)
			}
			fv.SetInt(v)
		}
	case "uint":
		if value != "" {
			v, err := strconv.ParseUint(value, 10, 64)
			if err != nil {
				return fmt.Errorf("failed to parse '%s' as uint", value)
			}
			fv.SetUint(v)
		}
	case "uint64":
		if value != "" {
			v, err := strconv.ParseUint(value, 10, 64)
			if err != nil {
				return fmt.Errorf("failed to parse '%s' as uint64", value)
			}
			fv.SetUint(v)
		}
	case "string":
		fv.SetString(value)
	case "[]bool":
		if value != "true" && value != "false" {
			return fmt.Errorf("failed to parse '%s' as bool", value)
		}
		if value == "true" {
			fv.Set(reflect.Append(fv, reflect.ValueOf(true)))
		} else if value == "false" {
			fv.Set(reflect.Append(fv, reflect.ValueOf(false)))
		}
	case "[]float64":
		if value != "" {
			v, err := strconv.ParseFloat(value, 64)
			if err != nil {
				return fmt.Errorf("failed to parse '%s' as float64", value)
			}
			fv.Set(reflect.Append(fv, reflect.ValueOf(v)))
		}
	case "[]int":
		if value != "" {
			v, err := strconv.ParseInt(value, 10, 32)
			if err != nil {
				return fmt.Errorf("failed to parse '%s' as int", value)
			}
			fv.Set(reflect.Append(fv, reflect.ValueOf(int(v))))
		}
	case "[]int64":
		if value != "" {
			v, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				return fmt.Errorf("failed to parse '%s' as int64", value)
			}
			fv.Set(reflect.Append(fv, reflect.ValueOf(v)))
		}
	case "[]uint":
		if value != "" {
			v, err := strconv.ParseUint(value, 10, 32)
			if err != nil {
				return fmt.Errorf("failed to parse '%s' as uint", value)
			}
			fv.Set(reflect.Append(fv, reflect.ValueOf(uint(v))))
		}
	case "[]uint64":
		if value != "" {
			v, err := strconv.ParseUint(value, 10, 64)
			if err != nil {
				return fmt.Errorf("failed to parse '%s' as uint64", value)
			}
			fv.Set(reflect.Append(fv, reflect.ValueOf(v)))
		}
	case "[]string":
		fv.Set(reflect.Append(fv, reflect.ValueOf(value)))
	default:
		return fmt.Errorf("invalid type %s. Supported types: %s", flag.valueType, supportedFlagTypes)
	}

	return nil
}

// unsetFlag sets a flag value to default by the given flag id
func (flagSet *FlagSet) unsetFlag(id int) error {
	if id < 0 {
		return errors.New("flag id is required")
	}

	// Check the flag
	flag := flagSet.lookupFlagByID(id)
	if flag == nil {
		return fmt.Errorf("no flag for id %d", id)
	}
	rValue := reflect.ValueOf(flagSet.flagsRaw).Elem()
	fv := rValue.FieldByIndex(flag.fieldIndex)
	if !fv.CanSet() {
		return fmt.Errorf("flag %s can't be set", flag.name)
	}

	// Set the value
	switch flag.valueType {
	case "bool":
		var v bool
		fv.SetBool(v)
	case "float64":
		var v float64
		fv.SetFloat(v)
	case "int":
		var v int64
		fv.SetInt(v)
	case "int64":
		var v int64
		fv.SetInt(v)
	case "uint":
		var v uint64
		fv.SetUint(v)
	case "uint64":
		var v uint64
		fv.SetUint(v)
	case "string":
		var v string
		fv.SetString(v)
	case "[]bool":
		fv.Set(reflect.Zero(reflect.TypeOf([]bool{})))
	case "[]float64":
		fv.Set(reflect.Zero(reflect.TypeOf([]float64{})))
	case "[]int":
		fv.Set(reflect.Zero(reflect.TypeOf([]int{})))
	case "[]int64":
		fv.Set(reflect.Zero(reflect.TypeOf([]int64{})))
	case "[]uint":
		fv.Set(reflect.Zero(reflect.TypeOf([]uint{})))
	case "[]uint64":
		fv.Set(reflect.Zero(reflect.TypeOf([]uint64{})))
	case "[]string":
		fv.Set(reflect.Zero(reflect.TypeOf([]string{})))
	default:
		return fmt.Errorf("invalid type %s. Supported types: %s", flag.valueType, supportedFlagTypes)
	}

	return nil
}

// structToFlags parses the given struct and return a list of flags
func structToFlags(value interface{}) ([]*Flag, []error) {
	// Init vars
	var result []*Flag

	// Iterate over the fields
	vType := reflect.Indirect(reflect.ValueOf(value)).Type()
	fields := typeToStructField(vType, nil)
	for k, field := range fields {
		// Init the flag
		flag := structFieldToFlag(field)
		flag.id = k
		flag.fieldIndex = field.index
		if field.parentIndex != nil {
			flag.parentIndex = field.parentIndex // vType.FieldByIndex(flag.parentIndex).Name
		}

		// Ignore non flag fields
		if flag.short == "" && flag.long == "" && flag.kind != "command" {
			continue
		}

		result = append(result, &flag)
	}

	// Check the flag arguments
	if errs := checkFlags(result); errs != nil {
		return nil, errs
	}

	// Iterate over the flags and set parent ids
	for _, v := range result {
		if v.parentIndex != nil {
			for _, vv := range result {
				if fmt.Sprint(v.parentIndex) == fmt.Sprint(vv.fieldIndex) { // faster then reflect.DeepEqual
					v.parentID = vv.id
				}
			}
		}
	}

	return result, nil
}

// structField represents a struct field
type structField struct {
	field       reflect.StructField
	index       []int
	parentIndex []int
}

// structFieldToFlag returns a new flag by the given struct field
func structFieldToFlag(sf structField) Flag {
	flag := Flag{
		id:           -1,
		name:         sf.field.Name,
		short:        strings.TrimSpace(sf.field.Tag.Get("short")),
		long:         strings.TrimSpace(sf.field.Tag.Get("long")),
		command:      strings.TrimSpace(sf.field.Tag.Get("command")),
		description:  strings.TrimSpace(sf.field.Tag.Get("description")),
		required:     false,
		env:          strings.TrimSpace(sf.field.Tag.Get("env")),
		delimiter:    sf.field.Tag.Get("delimiter"),
		valueDefault: strings.TrimSpace(sf.field.Tag.Get("default")),
		valueType:    sf.field.Type.String(),
		valueBy:      "",
		kind:         "arg",
		fieldIndex:   nil,
		parentIndex:  nil,
		parentID:     -1,
		commandID:    -1,
		args:         nil,
		err:          nil,
	}
	if r := sf.field.Tag.Get("required"); r == "true" {
		flag.required = true
	}

	// Cleanup args
	regArg, err := regexp.Compile("[^a-zA-Z0-9-_.]+")
	if err == nil {
		flag.short = regArg.ReplaceAllString(flag.short, "")
		flag.long = regArg.ReplaceAllString(flag.long, "")
		flag.command = regArg.ReplaceAllString(flag.command, "")
	}

	// Check commands
	if strings.HasPrefix(flag.valueType, "struct") {
		flag.valueType = "struct"
		flag.kind = "command"
		if flag.command == "" {
			flag.command = strings.ToLower(flag.name)
		}
	}

	return flag
}

// typeToStructField return a field list by the given reflect type
func typeToStructField(value reflect.Type, parentIndex []int) []structField {
	if value == nil {
		return nil
	}

	// Iterate over the fields
	var result []structField
	l := value.NumField()
	for i := 0; i < l; i++ {
		field := value.Field(i)
		sf := structField{
			field:       field,
			index:       append(parentIndex, field.Index...),
			parentIndex: parentIndex,
		}
		result = append(result, sf)

		// Check nested fields
		if strings.HasPrefix(field.Type.String(), "struct") {
			result = append(result, typeToStructField(field.Type, sf.index)...)
		}
	}

	return result
}

// checkFlags checks the flags for errors
func checkFlags(flags []*Flag) []error {
	// Init vars
	var result []error
	type f struct {
		name   string
		parent string
	}
	shorts := map[string]f{}
	longs := map[string]f{}
	commands := map[string]f{}

	// Iterate over the flags and check errors
	for _, v := range flags {

		// Duplicates and lengths
		parent := fmt.Sprint(v.parentIndex) // faster then reflect.DeepEqual
		if v.short != "" {
			if sf, ok := shorts[v.short]; ok && sf.parent == parent {
				result = append(result, fmt.Errorf("short argument %s in %s field is already defined in %s field", v.short, v.name, shorts[v.short].name))
			} else {
				if len(v.short) > 1 {
					result = append(result, fmt.Errorf("short argument %s in %s field must be one character long", v.short, v.name))
				} else {
					shorts[v.short] = f{name: v.name, parent: parent}
				}
			}
		}
		if v.long != "" {
			if lf, ok := longs[v.long]; ok && lf.parent == parent {
				result = append(result, fmt.Errorf("long argument %s in %s field is already defined in %s field", v.long, v.name, longs[v.long].name))
			} else {
				longs[v.long] = f{name: v.name, parent: parent}
			}
		}
		if v.command != "" {
			if cf, ok := commands[v.command]; ok && cf.parent == parent {
				result = append(result, fmt.Errorf("command %s in %s field is already defined in %s field", v.command, v.name, commands[v.command].name))
			} else {
				commands[v.command] = f{name: v.name, parent: parent}
			}
		}

		// Type
		ftFound := false
		for _, vv := range supportedFlagTypes {
			if v.valueType == vv {
				ftFound = true
				break
			}
		}
		if !ftFound {
			result = append(result, fmt.Errorf("invalid type %s. Supported types: %s", v.valueType, supportedFlagTypes))
		}
	}

	return result
}
