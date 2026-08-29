#!/usr/bin/env python3
import mmap
import os
import signal
import socket
import sys
import time


mode, pid_path, data_path = sys.argv[1:4]
with open(pid_path, "w", encoding="ascii") as pid_file:
    pid_file.write(f"{os.getpid()}\n")

if mode == "page-cache":
    fd = os.open(data_path, os.O_RDONLY)
    try:
        for _ in range(200_000):
            if not os.read(fd, 1):
                os.lseek(fd, 0, os.SEEK_SET)
                os.read(fd, 1)
    finally:
        os.close(fd)
elif mode == "socket":
    left, right = socket.socketpair()
    try:
        for _ in range(200_000):
            os.write(left.fileno(), b"x")
            os.read(right.fileno(), 1)
    finally:
        left.close()
        right.close()
elif mode in ("direct-read", "direct-write"):
    block_size = 4096
    file_size = 64 * 1024 * 1024
    flags = os.O_DIRECT | (os.O_RDONLY if mode == "direct-read" else os.O_RDWR | os.O_CREAT)
    fd = os.open(data_path, flags, 0o600)
    buffer = mmap.mmap(-1, block_size)
    buffer[:] = b"r" * block_size
    try:
        if mode == "direct-write":
            os.posix_fallocate(fd, 0, file_size)
        os.kill(os.getpid(), signal.SIGSTOP)
        offset = 0
        while True:
            if mode == "direct-read":
                os.preadv(fd, [buffer], offset)
            else:
                os.pwritev(fd, [buffer], offset)
            offset = (offset + block_size) % file_size
    finally:
        buffer.close()
        os.close(fd)
else:
    raise SystemExit(f"unknown workload mode: {mode}")

if mode in ("page-cache", "socket"):
    with open(f"{pid_path}.io", "w", encoding="ascii") as io_file:
        with open(f"/proc/{os.getpid()}/io", "r", encoding="ascii") as proc_io:
            io_file.write(proc_io.read())
    time.sleep(3)
