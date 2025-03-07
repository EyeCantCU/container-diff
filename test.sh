#!/bin/bash

# Copyright 2018 Google, Inc. All rights reserved.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

set -e

echo "Running go tests..."
go test -timeout 60s `go list ./... | grep -v vendor`

echo "Checking gofmt..."
files=$(find . -name "*.go" | grep -v vendor/ | xargs gofmt -l -s)
if [[ $files ]]; then
    echo "Gofmt errors in files: $files"
    exit 1
fi

echo "Checking go-addlicense..."
addlicense -check -l apache -ignore \{actions,differs,vendor,setup-tests,out\}/**/* -ignore *
