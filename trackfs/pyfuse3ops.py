#!/usr/bin/env python3
"""pyfuse3-based implementation of TrackFS operations.

This file implements a drop-in async backend that reproduces the behavior
of the existing fusepy-based TrackFSOps. It reuses fusepath.Factory and
TrackManager where possible and maps virtual names to inodes on demand.
"""

from __future__ import annotations

import asyncio
import errno
import logging
import os
import stat as statmod
from dataclasses import dataclass
from typing import Dict

import pyfuse3

from . import albuminfo
from .flactracks import TrackManager
from .fusepath import Factory, FusePath

log = logging.getLogger(__name__)


@dataclass
class OpenFileInfo:
    fd: int
    lock: asyncio.Lock


class PyTrackFS(pyfuse3.Operations):
    def __init__(
        self,
        root,
        keep_album=Factory.keep_album,
        separator=Factory.track_separator,
        album_extension=Factory.album_extension,
        title_length=Factory.max_title_len,
        tags_ignored=albuminfo.DEFAULT_IGNORE_TAGS_REX,
    ):
        super().__init__()
        self.root = os.path.realpath(root)
        self.keep_flac = keep_album
        self.tracks = TrackManager()
        self._fusepath_factory = Factory(
            track_separator=separator,
            max_title_len=title_length,
            album_extension=album_extension,
            keep_album=keep_album,
        )
        albuminfo.init(tags_ignored)

        # inode management
        self._inode_to_fp: Dict[int, FusePath] = {}
        self._fp_to_inode: Dict[str, int] = {}
        self._next_inode = pyfuse3.ROOT_INODE + 1
        self._inode_lock = asyncio.Lock()

        # root inode maps to the root directory (as FusePath with empty extension)
        root_fp = FusePath(self.root, "", False, None, None, self._fusepath_factory)
        self._inode_to_fp[pyfuse3.ROOT_INODE] = root_fp
        self._fp_to_inode[root_fp.source] = pyfuse3.ROOT_INODE

        # open file handles
        self._fh_counter = 1
        self._open_files: Dict[int, OpenFileInfo] = {}

    async def _alloc_inode(self, fp: FusePath) -> int:
        async with self._inode_lock:
            key = fp.source + (f"#{fp.num}" if fp.is_track and fp.num else "")
            if key in self._fp_to_inode:
                return self._fp_to_inode[key]
            ino = self._next_inode
            self._next_inode += 1
            self._inode_to_fp[ino] = fp
            self._fp_to_inode[key] = ino
            return ino

    def _attrs_from_stat(self, st, inode):
        # populate pyfuse3.EntryAttributes from os.stat_result
        entry = pyfuse3.EntryAttributes()
        entry.st_mode = st.st_mode
        entry.st_size = st.st_size
        entry.st_uid = st.st_uid
        entry.st_gid = st.st_gid
        entry.st_ino = inode
        entry.st_nlink = st.st_nlink
        entry.st_atime_ns = int(st.st_atime * 1e9)
        entry.st_mtime_ns = int(st.st_mtime * 1e9)
        entry.st_ctime_ns = int(st.st_ctime * 1e9)
        try:
            entry.st_blocks = st.st_blocks
            entry.st_blocksize = st.st_blksize
        except AttributeError:
            entry.st_blocks = 0
            entry.st_blocksize = 512
        return entry

    async def getattr(self, inode, ctx=None):
        log.info(f"getattr inode={inode}")
        if inode not in self._inode_to_fp:
            raise pyfuse3.FUSEError(errno.ENOENT)
        fp = self._inode_to_fp[inode]
        path = fp.source
        try:
            st = await asyncio.to_thread(os.lstat, path)
        except FileNotFoundError:
            raise pyfuse3.FUSEError(errno.ENOENT)
        entry = self._attrs_from_stat(st, inode)
        if fp.is_track:
            # adjust size for virtual track
            entry.st_size = self.tracks.estimate_track_file_size(fp.vpath, fp)
        return entry

    async def lookup(self, parent_inode, name, ctx=None):
        log.info(f"lookup parent={parent_inode} name={name}")
        if parent_inode not in self._inode_to_fp:
            raise pyfuse3.FUSEError(errno.ENOENT)
        parent_fp = self._inode_to_fp[parent_inode]
        # list entries in parent and try to find the matching name
        for entry in parent_fp.readdir():
            if entry == name:
                # construct child FusePath from the entry name
                child_base_fp = self._fusepath_factory.from_vpath(entry)
                # make source_root relative to parent directory
                child_source_root = os.path.join(
                    parent_fp.dirname(), child_base_fp.source_root
                )
                child_fp = FusePath(
                    child_source_root,
                    child_base_fp.extension,
                    child_base_fp.is_track,
                    child_base_fp.num,
                    child_base_fp.title,
                    child_base_fp._factory,
                )
                ino = await self._alloc_inode(child_fp)
                # get underlying stat
                try:
                    st = await asyncio.to_thread(os.lstat, child_fp.source)
                except FileNotFoundError:
                    raise pyfuse3.FUSEError(errno.ENOENT)
                entry_attr = self._attrs_from_stat(st, ino)
                if child_fp.is_track:
                    entry_attr.st_size = self.tracks.estimate_track_file_size(
                        entry, child_fp
                    )
                return entry_attr
        raise pyfuse3.FUSEError(errno.ENOENT)

    async def opendir(self, inode, ctx):
        # return a directory handle (we can reuse inode as handle)
        log.info(f"opendir inode={inode}")
        if inode not in self._inode_to_fp:
            raise pyfuse3.FUSEError(errno.ENOENT)
        return inode

    async def readdir(self, inode, off, token):
        log.info(f"readdir inode={inode} off={off}")
        if inode not in self._inode_to_fp:
            raise pyfuse3.FUSEError(errno.ENOENT)
        fp = self._inode_to_fp[inode]
        entries = fp.readdir()
        # entries already include '.' and '..' per fusepath.readdir
        idx = off
        # pyfuse3 expects offset 0..n; start after off
        while idx < len(entries):
            name = entries[idx]
            # allocate inode for this entry
            try:
                child_base_fp = self._fusepath_factory.from_vpath(name)
                child_source_root = os.path.join(
                    fp.dirname(), child_base_fp.source_root
                )
                child_fp = FusePath(
                    child_source_root,
                    child_base_fp.extension,
                    child_base_fp.is_track,
                    child_base_fp.num,
                    child_base_fp.title,
                    child_base_fp._factory,
                )
                ino = await self._alloc_inode(child_fp)
                # build attributes for readdir
                try:
                    st = await asyncio.to_thread(os.lstat, child_fp.source)
                    entry_attr = self._attrs_from_stat(st, ino)
                    if child_fp.is_track:
                        entry_attr.st_size = self.tracks.estimate_track_file_size(
                            name, child_fp
                        )
                except FileNotFoundError:
                    # fallback to directory entry with zeroed attributes
                    entry_attr = pyfuse3.EntryAttributes()
                    entry_attr.st_mode = statmod.S_IFREG | 0o444
                    entry_attr.st_size = 0
                    entry_attr.st_ino = ino
                if not pyfuse3.readdir_reply(
                    token, name.encode("utf-8"), entry_attr, idx + 1
                ):
                    return
            except Exception as e:
                log.exception("error in readdir for %s: %s", name, e)
            idx += 1

    async def readlink(self, inode, ctx=None):
        log.info(f"readlink inode={inode}")
        if inode not in self._inode_to_fp:
            raise pyfuse3.FUSEError(errno.ENOENT)
        fp = self._inode_to_fp[inode]
        try:
            return await asyncio.to_thread(os.readlink, fp.source)
        except OSError as e:
            raise pyfuse3.FUSEError(e.errno)

    async def statfs(self, inode):
        log.info(f"statfs inode={inode}")
        if inode not in self._inode_to_fp:
            raise pyfuse3.FUSEError(errno.ENOENT)
        fp = self._inode_to_fp[inode]
        path = fp.source
        stv = await asyncio.to_thread(os.statvfs, path)
        # return a dict-like object pyfuse3 expects
        return dict(
            f_bavail=stv.f_bavail,
            f_bfree=stv.f_bfree,
            f_blocks=stv.f_blocks,
            f_bsize=stv.f_bsize,
            f_favail=stv.f_favail,
            f_ffree=stv.f_ffree,
            f_files=stv.f_files,
            f_flag=stv.f_flag,
            f_frsize=stv.f_frsize,
            f_namemax=stv.f_namemax,
        )

    async def open(self, inode, flags, ctx):
        log.info(f"open inode={inode} flags={flags}")
        if inode not in self._inode_to_fp:
            raise pyfuse3.FUSEError(errno.ENOENT)
        fp = self._inode_to_fp[inode]
        # only allow read-only
        if (flags & os.O_RDONLY) == 0:
            raise pyfuse3.FUSEError(errno.EPERM)
        path = fp.source
        if fp.is_track:
            # prepare virtual track
            path = self.tracks.prepare_track(fp.vpath, fp)
        try:
            fd = await asyncio.to_thread(os.open, path, flags)
        except OSError as e:
            raise pyfuse3.FUSEError(e.errno)
        fh = self._fh_counter
        self._fh_counter += 1
        self._open_files[fh] = OpenFileInfo(fd=fd, lock=asyncio.Lock())
        return pyfuse3.FileInfo(fh=fh)

    async def read(self, fh, off, size):
        log.info(f"read fh={fh} off={off} size={size}")
        if fh not in self._open_files:
            raise pyfuse3.FUSEError(errno.EBADF)
        ofi = self._open_files[fh]
        async with ofi.lock:
            # do prefetch checks similar to original implementation
            # use os.pread to avoid changing shared file offset
            try:
                data = await asyncio.to_thread(os.pread, ofi.fd, size, off)
            except OSError as e:
                raise pyfuse3.FUSEError(e.errno)
            return data

    async def release(self, fh):
        log.info(f"release fh={fh}")
        if fh not in self._open_files:
            return
        ofi = self._open_files.pop(fh)
        try:
            await asyncio.to_thread(os.close, ofi.fd)
        except OSError:
            pass

    async def forget(self, inode_list):
        # called by kernel when it drops references; free inode cache if needed
        for inode, n in inode_list:
            if inode in self._inode_to_fp and inode != pyfuse3.ROOT_INODE:
                fp = self._inode_to_fp.pop(inode, None)
                key = fp.source + (
                    f"#{fp.num}" if fp and fp.is_track and fp.num else ""
                )
                self._fp_to_inode.pop(key, None)


# End of file
