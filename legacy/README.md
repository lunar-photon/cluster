# Legacy bash + C++ implementation

This is the previous version of `cluster`: a bash script (`cluster`) plus an
FTXUI dashboard (`src/main.cpp`). It is kept for reference while the Go
version (repository root) is validated on real clusters, and will be removed
afterwards.

Build the old dashboard with:

    cmake -B build -DCMAKE_BUILD_TYPE=Release && cmake --build build
