#!/usr/bin/env python3
#
# Copyright 2020-2021 by Andreas Schmidt
# All rights reserved.
# This file is part of the trackfs project
# and licensed under the terms of the GNU Lesser General Public License v3.0.
# See https://github.com/letwir/trackfs for details.
#
# This file is derived work of the FLACCue project.
# See https://github.com/acenko/FLACCue for details
#

from __future__ import absolute_import, division, print_function

import logging
import os
import stat
from collections import defaultdict
from dataclasses import dataclass
from threading import RLock

from fuse import Operations

from . import albuminfo, fusepath
from .flactracks import TrackManager

log = logging.getLogger(__name__)


@dataclass
class OpenFileInfo:
    position: int = 0
    lock = defaultdict(RLock)


class TrackFSOps(Operations):
    def __init__(
        self,
        root,
        keep_album=fusepath.DEFAULT_KEEP_ALBUM,
        separator=fusepath.DEFAULT_TRACK_SEPARATOR,
        album_extension=fusepath.DEFAULT_ALBUM_EXTENSION,
        title_length=fusepath.DEFAULT_MAX_TITLE_LEN,
        tags_ignored=albuminfo.DEFAULT_IGNORE_TAGS_REX,
    ):
        self.root = os.path.realpath(root)
        self.keep_flac = keep_album
        self.tracks = TrackManager()
        self._open_files = {}
        self._fusepath_factory = fusepath.Factory(
            track_separator=separator,
            max_title_len=title_length,
            album_extension=album_extension,
            keep_album=keep_album,
        )
        # TODO: avoid global init function
        albuminfo.init(tags_ignored)

    def __call__(self, op, path, *args):
        return super(TrackFSOps, self).__call__(op, self.root + path, *args)

    def _fusepath(self, path):
        return self._fusepath_factory.from_vpath(path)

    def getattr(self, path, fh=None):
        log.info(f"getattr for ({path}) [{fh}]")
        vpath = os.path.relpath(path, self.root)
        parts = self._fusepath_factory.split_vpath(vpath)
        # 仮想アルバムディレクトリ
        if parts[0] == "album":
            album = parts[1]
            realfile = os.path.join(self.root, album + ".flac")
            if os.path.exists(realfile):
                st = os.lstat(realfile)

                result = dict(
                    (key, getattr(st, key))
                    for key in (
                        "st_atime",
                        "st_ctime",
                        "st_gid",
                        "st_mtime",
                        "st_nlink",
                        "st_uid",
                    )
                )
                result["st_mode"] = stat.S_IFDIR | 0o755
                result["st_size"] = 4096
                return result
            fp = self._fusepath(path)
            if fp.is_track:
                realfile = self.resolve_album_file(fp.source_root)
                if realfile is None:
                    raise FileNotFoundError(path)
                st = os.lstat(realfile)
            else:
                st = os.lstat(fp.source)
        result = dict(
            (key, getattr(st, key))
            for key in (
                "st_atime",
                "st_ctime",
                "st_gid",
                "st_mode",
                "st_mtime",
                "st_nlink",
                "st_size",
                "st_uid",
            )
        )
        if fp.is_track:
            result["st_size"] = self.tracks.estimate_track_file_size(path, fp)
        return result

    def open(self, path, flags, *args, **pargs):
        log.info(f'open file "{path}"')
        # We don't want FlacTrackFS messing with actual data.
        # Only allow Read-Only access.
        if not (flags & os.O_RDONLY):
            raise ValueError("Can only open files read-only.")
        fp = self._fusepath(path)
        if fp.is_track:
            path = self.tracks.prepare_track(path, fp)
        log.debug(f'file to open = "{path}"')
        fh = os.open(path, flags, *args, **pargs)
        self._open_files[fh] = OpenFileInfo()
        log.debug(f'opened file file to open = "{path}" with fh [{fh}]')
        return fh

    def read(self, path, size, offset, fh):
        log.info(f"read from [{fh}] {offset} until {offset + size}")
        open_file_info = self._open_files[fh]
        # make sure that only one concurrent read per file handle is possible
        with open_file_info.lock:
            if open_file_info.position != offset:
                log.debug(f"out of band read; seek file to offset {offset}")
                os.lseek(fh, offset, 0)
            else:
                # we do preload-checks only on consecutive reads
                fp = self._fusepath(path)
                if fp.is_track:
                    self.tracks.check_next_track(path, fp, offset)
            open_file_info.position = offset + size
            return os.read(fh, size)

    def release(self, path, fh):
        log.info(f"release [{fh}] ({path})")
        del self._open_files[fh]
        fp = self._fusepath(path)
        if fp.is_track:
            self.tracks.release_track(path, fp)
        return os.close(fh)

    def resolve_album_file(path):
        if os.path.isfile(path):
            return path

        if os.path.isdir(path):
            for f in os.listdir(path):
                if f.lower().endswith((".flac", ".wav")):
                    return os.path.join(path, f)

        for ext in (".flac", ".wav"):
            p = path + ext
            if os.path.exists(p):
                return p

        return None

    def readdir(self, path, fh):
        log.info(f"readdir [{fh}] ({path})")
        vpath = os.path.relpath(path, self.root)
        if vpath == ".":
            vpath = ""
        parts = self._fusepath_factory.split_vpath(vpath)
        log.info(f"VPATH={vpath}")
        log.info(f"PARTS={parts}")

        entries = [".", ".."]
        # ROOT
        if parts[0] == "root":
            for filename in os.listdir(path):
                basename, ext = os.path.splitext(filename)
                if ext.lower() in [".flac", ".wav"]:
                    entries.append(basename)
                else:
                    entries.append(filename)
            log.info(f"READDIR ENTRIES={entries!r}")
            return entries
        # VIRTUAL ALBUM
        if parts[0] == "album":
            album = parts[1]
            realfile = None

            # album.flac style
            for ext in [".flac", ".wav"]:
                candidate = path + ext

                if os.path.exists(candidate):
                    realfile = candidate
                    break

            # directory album style
            if realfile is None and os.path.isdir(path):
                for filename in os.listdir(path):
                    if filename.lower().endswith((".flac", ".wav")):
                        realfile = os.path.join(path, filename)
                        break

            log.info(f"REALFILE={realfile}")

            if realfile is None:
                return entries

            trx = albuminfo.get(realfile).tracks()

            for t in trx:
                fp = self._fusepath_factory.from_track(
                    os.path.splitext(realfile)[0],
                    os.path.splitext(realfile)[1],
                    t,
                )

                entries.append(os.path.basename(fp.vpath))

            log.info(f"READDIR ENTRIES={entries!r}")
            return entries

    def readlink(self, path, *args, **pargs):
        log.info(f"readlink ({path})")
        path = self._fusepath(path).source
        return os.readlink(path, *args, **pargs)

    def statfs(self, path):
        log.info(f"statfs ({path})")
        path = self._fusepath(path).source
        stv = os.statvfs(path)
        return dict(
            (key, getattr(stv, key))
            for key in (
                "f_bavail",
                "f_bfree",
                "f_blocks",
                "f_bsize",
                "f_favail",
                "f_ffree",
                "f_files",
                "f_flag",
                "f_frsize",
                "f_namemax",
            )
        )
