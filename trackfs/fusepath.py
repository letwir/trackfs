#!/usr/bin/env python3
#
# Copyright 2020-2021 by Andreas Schmidt
# All rights reserved.
# This file is part of the trackfs project
# and licensed under the terms of the GNU Lesser General Public License v3.0.
# See https://github.com/andresch/trackfs for details.
#

#
# This module provides mapping of names between the virtual trackfs
# filesystem and the underlying root filesystem, esp. the naming of
# the individual track-files
#

import logging
import os
import re
import string
import unicodedata
from dataclasses import dataclass, field
from functools import cached_property

from . import albuminfo, cuesheet

log = logging.getLogger(__name__)

DEFAULT_TRACK_SEPARATOR: str = ".#-#."
DEFAULT_MAX_TITLE_LEN: int = 20
DEFAULT_ALBUM_EXTENSION: str = r"(?i:\.flac|\.wav)"
DEFAULT_VALID_CHARS: str = "-_() " + string.ascii_letters + string.digits
DEFAULT_KEEP_ALBUM: bool = False
DEFAULT_TRACK_EXTENSION: str = ".flac"


@dataclass(frozen=True)
class Factory:
    """manages the configuration options for the virtual fuse paths"""

    track_separator: str = DEFAULT_TRACK_SEPARATOR
    max_title_len: int = DEFAULT_MAX_TITLE_LEN
    extension: str = DEFAULT_ALBUM_EXTENSION
    valid_filename_chars: str = DEFAULT_VALID_CHARS
    keep_album: bool = DEFAULT_KEEP_ALBUM
    track_extension: bool = DEFAULT_TRACK_EXTENSION

    @cached_property
    def track_file_regex(self):
        ext_re = re.escape(self.track_extension)

        rex = (
            r"^(?P<basename>.*)/"
            r"(?P<num>\d+)"
            r"(?P<title>\.[^/]*)?" + ext_re + r"$"
        )

        log.debug("Factory.track_file_regex: " + rex)
        return re.compile(rex)

    @cached_property
    def album_ext_regex(self):
        return re.compile(self.album_extension)

    def from_track(self, source_root, extension, track):
        return FusePath(
            source_root=source_root,
            extension=extension,
            is_track=True,
            num=track.num,
            title=track.title,
            _factory=self,
        )

    def from_vpath(self, path):
        """Construct a FusePath instance from a given virtual path"""
        match = self.track_file_regex.match(path)
        if match is None:
            log.debug(f'no track file in "{path}"')
            (root, ext) = os.path.splitext(path)
            return FusePath(root, ext, _factory=self)
        log.debug(f'track file in "{path}"')
        title = match["title"].lstrip()
        return FusePath(
            match["basename"], ".flac", True, int(match["num"]), title, self
        )

    @staticmethod
    def split_vpath(path):

        parts = path.strip("/").split("/")

        if parts == [""] or len(parts) == 0:
            return ("root",)

        filename = parts[-1]

        if re.match(r"^\d+.*\.flac$", filename):
            return ("track", "/".join(parts[:-1]), filename)

        return ("album", "/".join(parts))


_DEFAULT_FACTORY = Factory()


@dataclass(frozen=True)
class FusePath:
    """represents an entry in the virtual trackfs filesystem"""

    source_root: str
    extension: str
    is_track: bool = False
    num: int = None
    title: str = None
    _factory: Factory = _DEFAULT_FACTORY
    real_source: str = None

    @property
    def track_separator(self):
        return self._factory.track_separator

    @property
    def max_title_len(self):
        return self._factory.max_title_len

    @property
    def flac_extension(self):
        return self._factory.album_extension

    @property
    def valid_filename_chars(self):
        return self._factory.valid_filename_chars

    @property
    def track_file_regex(self):
        return self._factory.track_file_regex

    @property
    def album_ext_regex(self):
        return self._factory.album_ext_regex

    @property
    def keep_album(self):
        return self._factory.keep_album

    @property
    def track_extension(self):
        return self._factory.track_extension

    @cached_property
    def source(self):
        if self.real_source:
            return self.real_source

        candidate = self.source_root + self.extension
        if os.path.exists(candidate):
            return candidate

        # fallback
        for f in os.listdir(self.source_root):
            if f.lower().endswith(self.extension):
                return os.path.join(self.source_root, f)

        raise FileNotFoundError(self.source_root)

    @property
    def title_fragment(self):
        """the fragment of a track's title that goes into a vpath"""
        if self.title is None or len(self.title) == 0:
            return ""
        else:
            clean_title = unicodedata.normalize("NFKD", self.title)[
                : self.max_title_len
            ]
            return "." + "".join(
                "_" if c in "[]\\/:*?%&$'`\"<>|+" else c for c in clean_title
            )

    @property
    def vpath(self):
        if self.is_track:
            return (
                f"{self.source_root}/{self.num:03d}"
                f"{self.title_fragment}{self.track_extension}"
            )
        else:
            return self.source

    def dirname(self):
        return os.path.dirname(self.source_root)

    def for_other_track(
        self, num: int, title: str, start: cuesheet.Time, end: cuesheet.Time
    ):
        """Construct fusepath entry for another track of the same FLAC+CUE file"""
        return FusePath(
            self.source_root, self.extension, True, num, title, self._factory
        )
