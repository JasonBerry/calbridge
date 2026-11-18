package spike

import (
	"context"
	"errors"
)

var errM3 = errors.New("not implemented yet (M3)")

func caldavDiscover(context.Context, *env, []string) error { return errM3 }
func caldavPut(context.Context, *env, []string) error      { return errM3 }
func caldavGet(context.Context, *env, []string) error      { return errM3 }
func caldavSync(context.Context, *env, []string) error     { return errM3 }
func caldavDelete(context.Context, *env, []string) error   { return errM3 }
func caldavVolume(context.Context, *env, []string) error   { return errM3 }
