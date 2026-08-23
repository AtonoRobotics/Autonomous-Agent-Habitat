from google.protobuf.internal import containers as _containers
from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from collections.abc import Iterable as _Iterable, Mapping as _Mapping
from typing import ClassVar as _ClassVar, Optional as _Optional, Union as _Union

DESCRIPTOR: _descriptor.FileDescriptor

class GetCandidateRequest(_message.Message):
    __slots__ = ("id",)
    ID_FIELD_NUMBER: _ClassVar[int]
    id: str
    def __init__(self, id: _Optional[str] = ...) -> None: ...

class ListCandidatesRequest(_message.Message):
    __slots__ = ("candidate_class", "status")
    CANDIDATE_CLASS_FIELD_NUMBER: _ClassVar[int]
    STATUS_FIELD_NUMBER: _ClassVar[int]
    candidate_class: str
    status: str
    def __init__(self, candidate_class: _Optional[str] = ..., status: _Optional[str] = ...) -> None: ...

class ListCandidatesResponse(_message.Message):
    __slots__ = ("candidates",)
    CANDIDATES_FIELD_NUMBER: _ClassVar[int]
    candidates: _containers.RepeatedCompositeFieldContainer[CandidateVersion]
    def __init__(self, candidates: _Optional[_Iterable[_Union[CandidateVersion, _Mapping]]] = ...) -> None: ...

class CandidateVersion(_message.Message):
    __slots__ = ("id", "candidate_class", "ref", "status", "generated_by", "created_at", "canary_at", "promoted_at", "demoted_at", "rolled_back_at", "rollback_target_id")
    ID_FIELD_NUMBER: _ClassVar[int]
    CANDIDATE_CLASS_FIELD_NUMBER: _ClassVar[int]
    REF_FIELD_NUMBER: _ClassVar[int]
    STATUS_FIELD_NUMBER: _ClassVar[int]
    GENERATED_BY_FIELD_NUMBER: _ClassVar[int]
    CREATED_AT_FIELD_NUMBER: _ClassVar[int]
    CANARY_AT_FIELD_NUMBER: _ClassVar[int]
    PROMOTED_AT_FIELD_NUMBER: _ClassVar[int]
    DEMOTED_AT_FIELD_NUMBER: _ClassVar[int]
    ROLLED_BACK_AT_FIELD_NUMBER: _ClassVar[int]
    ROLLBACK_TARGET_ID_FIELD_NUMBER: _ClassVar[int]
    id: str
    candidate_class: str
    ref: str
    status: str
    generated_by: str
    created_at: str
    canary_at: str
    promoted_at: str
    demoted_at: str
    rolled_back_at: str
    rollback_target_id: str
    def __init__(self, id: _Optional[str] = ..., candidate_class: _Optional[str] = ..., ref: _Optional[str] = ..., status: _Optional[str] = ..., generated_by: _Optional[str] = ..., created_at: _Optional[str] = ..., canary_at: _Optional[str] = ..., promoted_at: _Optional[str] = ..., demoted_at: _Optional[str] = ..., rolled_back_at: _Optional[str] = ..., rollback_target_id: _Optional[str] = ...) -> None: ...
