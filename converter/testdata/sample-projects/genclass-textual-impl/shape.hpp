#pragma once
struct Shape {
  int area() const;
  int perimeter() const;
};
#include "shape_impl.inl"
#include "shape_impl_extra.cc"
