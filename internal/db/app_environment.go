package db

import (
	"fmt"
	"strings"
)

// dropURLAppEnvironment removes every DATABASE_URL spelling of app.environment (setting names are
// case-insensitive) and refuses an `options` startup parameter that names it: the value always
// comes from the application configuration (config.GuardEnvironment, ADR 0115 S1), never from a
// connection string. This is hygiene, not the security control: any session can still set_config()
// the setting, which is why the database's synthetic-configuration guard also requires the
// owner-written database_environment anchor (migration 0135).
func dropURLAppEnvironment(rp map[string]string) error {
	for k, v := range rp {
		lk := strings.ToLower(k)
		if lk == "options" {
			if strings.Contains(strings.ToLower(v), appEnvironmentParam) {
				return fmt.Errorf("db: database url sets %s through the options parameter; it is set from the application configuration only", appEnvironmentParam)
			}
			continue
		}
		if lk == appEnvironmentParam {
			delete(rp, k)
		}
	}
	return nil
}
