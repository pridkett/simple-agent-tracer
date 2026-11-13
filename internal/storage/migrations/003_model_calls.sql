CREATE INDEX model_call_order ON spans(project, sort_time DESC, trace DESC, id DESC)
 WHERE model_call=TRUE AND linked_model=FALSE;
