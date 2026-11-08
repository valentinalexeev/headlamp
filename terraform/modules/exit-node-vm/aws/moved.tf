# The key pair became optional (count). Existing state has it without an index: move it, so the plan keeps it.
moved {
  from = aws_key_pair.this
  to   = aws_key_pair.this[0]
}
