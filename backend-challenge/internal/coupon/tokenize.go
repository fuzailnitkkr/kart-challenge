package coupon

import (
	"context"
	"errors"
	"io"
)

// visitNormalizedCodes streams uppercase alphanumeric coupon candidates
// (length 8..10) from reader to visitor.
func visitNormalizedCodes(ctx context.Context, reader io.Reader, visitor func(code string) (stop bool, err error)) error {
	if visitor == nil {
		return errors.New("visitor is required")
	}

	buf := make([]byte, 64*1024)
	token := make([]byte, 0, maxCouponLength)
	tooLong := false

	emit := func() error {
		if tooLong || len(token) < minCouponLength || len(token) > maxCouponLength {
			token = token[:0]
			tooLong = false
			return nil
		}

		stop, err := visitor(string(token))
		token = token[:0]
		tooLong = false
		if err != nil {
			return err
		}
		if stop {
			return io.EOF
		}
		return nil
	}

	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		n, err := reader.Read(buf)
		if n > 0 {
			for i := 0; i < n; i++ {
				b := buf[i]
				if isAlphaNum(b) {
					if tooLong {
						continue
					}
					if len(token) < maxCouponLength {
						token = append(token, toUpperASCII(b))
					} else {
						tooLong = true
					}
					continue
				}

				if emitErr := emit(); emitErr != nil {
					if errors.Is(emitErr, io.EOF) {
						return nil
					}
					return emitErr
				}
			}
		}

		if err != nil {
			if errors.Is(err, io.EOF) {
				if emitErr := emit(); emitErr != nil && !errors.Is(emitErr, io.EOF) {
					return emitErr
				}
				return nil
			}
			return err
		}
	}
}

func toUpperASCII(b byte) byte {
	if b >= 'a' && b <= 'z' {
		return b - ('a' - 'A')
	}
	return b
}
