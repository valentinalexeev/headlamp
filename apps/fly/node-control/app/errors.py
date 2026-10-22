"""Errors shared by every provider module."""


class NotConfigured(Exception):
    """A required setting, credential or per-node config block is missing."""


class APIError(Exception):
    """The cloud provider's API call itself failed."""
