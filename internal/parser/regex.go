// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 Przemysław Malkowski
//
// This program is free software: you can redistribute it and/or modify it
// under the terms of the GNU General Public License as published by the Free
// Software Foundation, either version 3 of the License, or (at your option)
// any later version. See the LICENSE file for the full license text.

package parser

import "regexp"

// mustMatch compiles a regexp at package-init time, panicking on a bad pattern
// (which would be a programming error, caught immediately in tests).
func mustMatch(pattern string) *regexp.Regexp {
	return regexp.MustCompile(pattern)
}
