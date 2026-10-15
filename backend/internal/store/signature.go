package store

import "reflect"

// Keep inode and change time without coupling the importer to a platform's
// syscall.Stat_t layout. Access time must not invalidate a signature on reads.
func signatureStat(stat any) map[string]any {
	v := reflect.ValueOf(stat)
	if v.Kind() == reflect.Pointer {
		v = v.Elem()
	}
	out := make(map[string]any)
	if v.Kind() != reflect.Struct {
		return out
	}
	for _, name := range []string{"Dev", "Ino", "Ctim", "Ctimespec", "Ctime", "Ctimensec"} {
		if f := v.FieldByName(name); f.IsValid() && f.CanInterface() {
			out[name] = f.Interface()
		}
	}
	return out
}
