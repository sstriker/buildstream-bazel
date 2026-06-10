# The alpha lib — declared at the top of an included file.
add_library(alpha STATIC src/alpha.c)

# The beta lib — second target in the same included file.
add_library(beta STATIC src/beta.c)
