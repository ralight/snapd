// -*- Mode: Go; indent-tabs-mode: t -*-

/*
 * Copyright (C) 2019 Canonical Ltd
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License version 3 as
 * published by the Free Software Foundation.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 *
 */

package main_test

import (
	. "gopkg.in/check.v1"

	snap "github.com/snapcore/snapd/cmd/snap"
	"github.com/snapcore/snapd/image"
)

type SnapPrepareImageSuite struct {
	BaseSnapSuite
}

var _ = Suite(&SnapPrepareImageSuite{})

func (s *SnapPrepareImageSuite) TestPrepareImageCore(c *C) {
	var opts *image.Options
	prep := func(o *image.Options) error {
		opts = o
		return nil
	}
	r := snap.MockImagePrepare(prep)
	defer r()

	rest, err := snap.Parser(snap.Client()).ParseArgs([]string{"prepare-image", "model", "root-dir"})
	c.Assert(err, IsNil)
	c.Assert(rest, DeepEquals, []string{})

	c.Check(opts, DeepEquals, &image.Options{
		ModelFile:       "model",
		RootDir:         "root-dir/image",
		GadgetUnpackDir: "root-dir/gadget",
	})
}

func (s *SnapPrepareImageSuite) TestPrepareImageClassic(c *C) {
	var opts *image.Options
	prep := func(o *image.Options) error {
		opts = o
		return nil
	}
	r := snap.MockImagePrepare(prep)
	defer r()

	rest, err := snap.Parser(snap.Client()).ParseArgs([]string{"prepare-image", "--classic", "model", "root-dir"})
	c.Assert(err, IsNil)
	c.Assert(rest, DeepEquals, []string{})

	c.Check(opts, DeepEquals, &image.Options{
		Classic:   true,
		ModelFile: "model",
		RootDir:   "root-dir",
	})
}

func (s *SnapPrepareImageSuite) TestPrepareImageClassicArch(c *C) {
	var opts *image.Options
	prep := func(o *image.Options) error {
		opts = o
		return nil
	}
	r := snap.MockImagePrepare(prep)
	defer r()

	rest, err := snap.Parser(snap.Client()).ParseArgs([]string{"prepare-image", "--classic", "--arch", "i386", "model", "root-dir"})
	c.Assert(err, IsNil)
	c.Assert(rest, DeepEquals, []string{})

	c.Check(opts, DeepEquals, &image.Options{
		Classic:      true,
		Architecture: "i386",
		ModelFile:    "model",
		RootDir:      "root-dir",
	})
}

func (s *SnapPrepareImageSuite) TestPrepareImageExtraSnaps(c *C) {
	var opts *image.Options
	prep := func(o *image.Options) error {
		opts = o
		return nil
	}
	r := snap.MockImagePrepare(prep)
	defer r()

	rest, err := snap.Parser(snap.Client()).ParseArgs([]string{"prepare-image", "model", "root-dir", "--channel", "candidate", "--snap", "foo", "--snap", "bar=t/edge", "--snap", "local.snap", "--extra-snaps", "local2.snap", "--extra-snaps", "store-snap"})
	c.Assert(err, IsNil)
	c.Assert(rest, DeepEquals, []string{})

	c.Check(opts, DeepEquals, &image.Options{
		ModelFile:       "model",
		Channel:         "candidate",
		RootDir:         "root-dir/image",
		GadgetUnpackDir: "root-dir/gadget",
		Snaps:           []string{"foo", "bar", "local.snap", "local2.snap", "store-snap"},
		SnapChannels:    map[string]string{"bar": "t/edge"},
	})
}
