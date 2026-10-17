.PHONY: all build clean install

all: build

build:
	cmake -B build -DCMAKE_BUILD_TYPE=Release
	cmake --build build -j$$(nproc)

install: build
	./install.sh

clean:
	rm -rf build
