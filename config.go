package main

/* p4unity
 * `change-content` handler for Perforce Helix to guard against
 * bad behaviour with Unity projects' .meta files
 *
 * harry denholm, 2020; ishani.org
 */

import (
	"io/ioutil"
	"log"
	"os"
	"reflect"
	"strconv"

	"github.com/BurntSushi/toml"
)

type tomlConfig struct {
	VerboseLogs     bool     `toml:"verbose_logs" env:"P4U_VERBOSE"`
	PerforceServer  string   `toml:"perforce_server" env:"P4U_SERVER"`
	PerforceUser    string   `toml:"perforce_user" env:"P4U_USER"`
	PerforcePass    string   `toml:"perforce_pass" env:"P4U_PASS"`
	BypassKeyphrase string   `toml:"bypass_keyphrase" env:"P4U_BYPASS"`
	PathWhitelist   []string `toml:"path_whitelist"`
}

// AppConfig is the config data parsed from disk
var AppConfig tomlConfig

// LoadConfig fetches current settings from the toml file on disk
func LoadConfig() {

	configFilename := "p4unity.toml"

	cfgBytes, err := ioutil.ReadFile(configFilename)
	if err != nil {
		log.Panicf("[p4unity:config] p4unity.toml not found - %s", err)
	}

	// parse and map the data onto the structs
	if _, err := toml.Decode(string(cfgBytes), &AppConfig); err != nil {
		log.Panicf("[p4unity:config] Decode failure - %s", err)
	}

	// loop throught the config fields; anything with an 'env' tag allows for override with envvars
	if err = checkOverrides(&AppConfig); err != nil {
		log.Panicf("[p4unity:config] Override failure - %s", err)
	}
}

func checkOverrides(configData interface{}) error {

	var err error

	smType := reflect.TypeOf(configData).Elem()
	smValue := reflect.ValueOf(configData).Elem()

	// walk each field, look for 'env' items
	for i := 0; i < smType.NumField(); i++ {

		fieldType := smType.Field(i)
		field := smValue.Field(i)

		envOverride := fieldType.Tag.Get("env")

		if envOverride != "" {

			overrideFromEnv := os.Getenv(envOverride)

			// log.Printf("%s => %s", envOverride, overrideFromEnv)

			if overrideFromEnv != "" {

				switch field.Kind() {
				case reflect.String:
					field.Set(reflect.ValueOf(overrideFromEnv))

				case reflect.Int32:
					ivalue, err := strconv.ParseInt(overrideFromEnv, 0, 32)
					if err != nil {
						return err
					}
					field.Set(reflect.ValueOf(int32(ivalue)))

				case reflect.Float64:
					fvalue, err := strconv.ParseFloat(overrideFromEnv, 64)
					if err != nil {
						return err
					}
					field.Set(reflect.ValueOf(float64(fvalue)))

				case reflect.Bool:
					bvalue, err := strconv.ParseBool(overrideFromEnv)
					if err != nil {
						return err
					}
					field.Set(reflect.ValueOf(bvalue))
				}

			}
		}

		if field.Kind().String() == "struct" {
			vx := smValue.Field(i).Addr()
			err = checkOverrides(vx.Interface())
			if err != nil {
				return err
			}
		}
	}
	return nil
}
