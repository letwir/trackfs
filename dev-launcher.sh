#!/bin/sh
# 
# Copyright 2020 by Andreas Schmidt
# All rights reserved.
# This file is part of the trackfs project
# and licensed under the terms of the GNU Lesser General Public License v3.0.
# See https://github.com/andresch/trackfs for details.
#

# tell fusepy where to find fuse library
export FUSE_LIBRARY_PATH=/usr/lib/libfuse.so
# make sure that python doesn't write into the mounted working directory
export PYTHONPYCACHEPREFIX=/tmp/__pycache__

# prepare the dev tools for trackfs
ln -sf /work/.pypirc ~/.pypirc

# prepare the Python virtual environment in the workspace
if [ ! -x /work/.venv/bin/python ]; then
    virtualenv --clear /work/.venv
fi
export VIRTUAL_ENV=/work/.venv
export PATH="/work/.venv/bin:${PATH}"

# install the project into the virtual environment
/work/.venv/bin/python -m pip install -e /work

# launch sub-shell
export ENV=/work/tools.sh
/bin/sh

