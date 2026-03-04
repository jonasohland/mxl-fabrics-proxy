cmake_minimum_required(VERSION 3.24 FATAL_ERROR)

project(mxl-fabrics-proxy VERSION 0.0.1)

find_package(PkgConfig REQUIRED)
find_package(mxl CONFIG REQUIRED COMPONENTS)
find_package(spdlog CONFIG REQUIRED)
find_package(nlohmann_json CONFIG REQUIRED)
pkg_check_modules(libfabric REQUIRED IMPORTED_TARGET libfabric)

add_executable(mxl-fabrics-proxy-worker)

target_link_libraries(
	mxl-fabrics-proxy-worker PRIVATE 
		mxl::mxl
        mxl::mxl-common
		mxl::mxl-fabrics
        spdlog::spdlog
        nlohmann_json::nlohmann_json
)

target_sources(mxl-fabrics-proxy-worker
	PRIVATE
		"src/main.cpp"
        "src/mxl.cpp"
        "src/fabrics.cpp"
        "src/config.cpp"
        "src/initiator.cpp"
        "src/target.cpp"
        "src/util.cpp"
)

target_compile_features(mxl-fabrics-proxy-worker
    PRIVATE
        cxx_std_20
)

configure_file(
    "src/version.h.in"
    "${CMAKE_CURRENT_BINARY_DIR}/version_header/version.h"
)

target_include_directories(mxl-fabrics-proxy-worker
    PRIVATE
        "${CMAKE_CURRENT_BINARY_DIR}/version_header"
)

include(GNUInstallDirs)
install(TARGETS mxl-fabrics-proxy-worker DESTINATION "${CMAKE_INSTALL_BINDIR}")
