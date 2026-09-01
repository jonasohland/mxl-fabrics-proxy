cmake_minimum_required(VERSION 3.24 FATAL_ERROR)

project(mxl-replicator VERSION 0.0.1)

find_package(PkgConfig REQUIRED)
find_package(mxl CONFIG REQUIRED COMPONENTS)
find_package(spdlog CONFIG REQUIRED)
find_package(nlohmann_json CONFIG REQUIRED)
pkg_check_modules(libfabric REQUIRED IMPORTED_TARGET libfabric)
pkg_check_modules(libuuid REQUIRED IMPORTED_TARGET uuid)

add_executable(mxl-replicator-worker)

target_link_libraries(
	mxl-replicator-worker PRIVATE 
	mxl::mxl
	mxl::mxl-fabrics
        spdlog::spdlog
        nlohmann_json::nlohmann_json
        PkgConfig::libfabric
        PkgConfig::libuuid
)

target_sources(mxl-replicator-worker
	PRIVATE
		"src/main.cpp"
        "src/mxl.cpp"
        "src/fabrics.cpp"
        "src/config.cpp"
        "src/initiator.cpp"
        "src/target.cpp"
        "src/util.cpp"
        "src/metrics.cpp"
        "src/summary.cpp"
        "src/rt.cpp"
)

target_compile_features(mxl-replicator-worker
    PRIVATE
        cxx_std_20
)

configure_file(
    "src/version.h.in"
    "${CMAKE_CURRENT_BINARY_DIR}/version_header/version.h"
)

target_include_directories(mxl-replicator-worker
    PRIVATE
        "${CMAKE_CURRENT_BINARY_DIR}/version_header"
)

# Mock producer and consumer for end-to-end tests.
#
# They link mxl::mxl but not mxl::mxl-fabrics: they are media functions, not
# replication, and the point of them is to exercise the SDK path that
# mxl-utils' synthetic testutil flows cannot reach. They reuse the worker's own
# SDK wrapper (src/mxl.cpp) rather than a second one, so a change to how this
# project drives the library is exercised by the tools that test it.
#
# Built by default and installed, because the end-to-end suite runs them from
# the image. They are small and add no dependency the worker does not already
# have.
foreach(tool src sink)
    add_executable("mxl-mock-${tool}")

    target_link_libraries("mxl-mock-${tool}"
        PRIVATE
            mxl::mxl
            spdlog::spdlog
            nlohmann_json::nlohmann_json
            PkgConfig::libuuid
    )

    target_sources("mxl-mock-${tool}"
        PRIVATE
            "src/mock/${tool}.cpp"
            "src/mock/args.cpp"
            "src/mock/output.cpp"
            "src/mock/pattern.cpp"
            "src/mxl.cpp"
            "src/util.cpp"
    )

    target_compile_features("mxl-mock-${tool}"
        PRIVATE
            cxx_std_20
    )
endforeach()

include(GNUInstallDirs)
install(
    TARGETS
        mxl-replicator-worker
        mxl-mock-src
        mxl-mock-sink
    DESTINATION "${CMAKE_INSTALL_BINDIR}"
)
