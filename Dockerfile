# =================================
# Dockerfile for trackfs
# 
# Copyright 2020 by Andreas Schmidt
# All rights reserved.
# This file is part of the trackfs project
# and licensed under the terms of the GNU Lesser General Public License v3.0.
# See https://github.com/andresch/trackfs for details.
#
# =================================

FROM docker.io/python:3.8-alpine as builder

# build flac1.5.0
RUN \
  apk update \
  && apk --no-cache add alpine-sdk git cmake doxygen pandoc \
  && git clone https://github.com/xiph/flac.git -b 1.5.0 \
  && cd flac \
  && git clone https://github.com/xiph/ogg.git \
  && echo -e "\ntarget_link_libraries(replaygain_analysis m)" >> src/share/replaygain_analysis/CMakeLists.txt \
  && cmake . \
  && make -j $(nproc) \
  && make install \
  && cd ../
# install dependencies  
RUN \
  apk --no-cache add fuse fuse-dev \
  && /usr/local/bin/python -m pip install --upgrade pip

# enable non-root users to make FUSE fs non-private
RUN echo "user_allow_other" >> /etc/fuse.conf 

# FUSE requires that the user that mounts the FUSE filesystem
# has an entry in /etc/passwd
# Since we want to allow (and encourage) the usage of docker's
# --user option to run the container as non-root user, 
# and with that don't know the uid of the user at build time
# we can't create the entry for that user at build time
# and also can't use adduser command during runtime as this would
# require root privileges.
# Instead we open /etc/passwd for writing. 
# As /ets/shadow is still protected this should not cause harm,
# even if some attacker finds a way to take over the container

RUN chmod 666 /etc/passwd 

# Ensure that we get a docker image cache invalidation when there's new content available
ADD https://api.github.com/repos/letwir/trackfs/compare/master...HEAD /dev/null

# Now install the latest trackfs version from pypi
RUN \
  apk --no-cache add gcc python3-dev musl-dev linux-headers \
  && pip install psutil \
  && pip install git+https://github.com/letwir/trackfs/ --break-system-packages

# Remove build kit
RUN \
  apk del gcc python3-dev musl-dev linux-headers alpine-sdk git cmake doxygen pandoc

# source directory containing flac+cue files
VOLUME /src

# mount point where to generate the tracks from the flac+cue files
VOLUME /dst

COPY launcher.sh /usr/local/bin/
RUN chmod 555 /usr/local/bin/launcher.sh

ENTRYPOINT ["/usr/local/bin/launcher.sh", "-j $(nproc)", "/src", "/dst"]


