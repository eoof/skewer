package encoders

import (
	"io"

	"github.com/stephane-martin/skewer/model"
)

func encodePB(v interface{}, w io.Writer) error {
	if v == nil {
		return nil
	}
	switch val := v.(type) {
	case *model.FullMessage:
		buf, err := val.Marshal()
		if err != nil {
			return err
		}
		_, err = w.Write(buf)
		return err
	case *model.ParsedMessage:
		buf, err := val.Marshal()
		if err != nil {
			return err
		}
		_, err = w.Write(buf)
		return err
	case *model.SyslogMessage:
		buf, err := val.Marshal()
		if err != nil {
			return err
		}
		_, err = w.Write(buf)
		return err
	default:
		return defaultEncode(v, w)
	}
}
