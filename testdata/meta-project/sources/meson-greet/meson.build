project('greet', 'c', version: '0.1.0', default_options: ['c_std=c11'])

inc = include_directories('include')

libgreet = static_library(
    'greet',
    'src/greet.c',
    include_directories: inc,
    install: true,
)

executable(
    'greet-bin',
    'src/main.c',
    link_with: libgreet,
    include_directories: inc,
    install: true,
)
