#!/usr/bin/env python
# Copyright (c) 2014-2016 Tigera, Inc. All rights reserved.
#
# All Rights Reserved.
#
#    Licensed under the Apache License, Version 2.0 (the "License"); you may
#    not use this file except in compliance with the License. You may obtain
#    a copy of the License at
#
#         http://www.apache.org/licenses/LICENSE-2.0
#
#    Unless required by applicable law or agreed to in writing, software
#    distributed under the License is distributed on an "AS IS" BASIS, WITHOUT
#    WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied. See the
#    License for the specific language governing permissions and limitations
#    under the License.

import inspect
import os
import os.path
import setuptools


def collect_requirements():
    # This monstrosity is the only way to definitely get the location of
    # setup.py regardless of how you execute it. It's tempting to use __file__
    # but that only works if setup.py is executed directly, otherwise it all
    # goes terribly wrong.
    directory = os.path.dirname(
        os.path.abspath(inspect.getfile(inspect.currentframe()))
    )
    reqfile = os.path.join(directory, "requirements.txt")
    reqs = set()
    with open(reqfile, 'r') as f:
        for line in f:
            line = line.split('#', 1)[0].strip()
            if line:
                reqs.add(line)

    return reqs


packages = setuptools.find_packages()
requirements = collect_requirements()

setuptools.setup(
    name="felix",
    version="2.0.0-beta2",
    packages=setuptools.find_packages(),
    entry_points={
        'console_scripts': [
            'calico-iptables-plugin = calico.felix.felix:main',
            'calico-dummydp-plugin = calico.felix.dummydp:main',
            'calico-cleanup = calico.felix.cleanup:main',
        ],
        'calico.felix.iptables_generator': [
            'default = calico.felix.plugins.fiptgenerator:FelixIptablesGenerator',
        ],
    },
    scripts=['utils/calico-diags'],
    install_requires=requirements
)
