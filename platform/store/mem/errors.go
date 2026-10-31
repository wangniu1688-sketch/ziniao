package mem

import (
	"fmt"
	"net/http"

	"ziniao/platform/apierr"
)

func notFound(kind, id string) error {
	return apierr.New(apierr.CodeNotFound, http.StatusNotFound, fmt.Sprintf("%s 不存在: %s", kind, id))
}

func conflict(kind, id string) error {
	return apierr.New(apierr.CodeConflict, http.StatusConflict, fmt.Sprintf("%s 已存在: %s", kind, id))
}
