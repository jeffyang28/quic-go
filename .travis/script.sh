#!/usr/bin/env bash

set -ex

go get -t ./...
if [ ${TESTMODE} == "unit" ]; then
  ginkgo -r -v -cover -randomizeAllSpecs -randomizeSuites -trace -skipPackage integrationtests,benchmark
fi

if [ ${TESTMODE} == "integration" ]; then
  # run benchmark tests
  ginkgo -randomizeAllSpecs -randomizeSuites -trace benchmark -- -samples=1
  # run integration tests
  ginkgo -r -v -randomizeAllSpecs -randomizeSuites -trace integrationtests
fi

if [ ${TESTMODE} == "race_integration" ]; then  
  # run benchmark tests
  ginkgo -race -randomizeAllSpecs -randomizeSuites -trace benchmark -- -samples=1 -size=10
  # run integration tests
  ginkgo -race -v -r -randomizeAllSpecs -randomizeSuites -trace integrationtests
fi
