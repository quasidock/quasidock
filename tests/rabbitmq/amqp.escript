#!/usr/bin/env escript
%%! -noshell
-mode(compile).
-include_lib("amqp_client/include/amqp_client.hrl").
main([Mode]) ->
    {ok, JSON} = file:read_file(filename:join(os:getenv("RABBITMQ_BASE"), "credentials.json")),
    #{<<"username">> := User, <<"password">> := Password} = json:decode(JSON),
    Secret = case Mode of "unauthorized" -> <<"incorrect-password">>; _ -> Password end,
    Result = amqp_connection:start(#amqp_params_network{
        host = "127.0.0.1", port = list_to_integer(os:getenv("RABBITMQ_NODE_PORT")),
        username = User, password = Secret}),
    case Result of
        {error, _} when Mode =:= "unauthorized" -> erlang:display(unauthorized_ok), halt();
        _ -> ok
    end,
    {ok, Connection} = Result,
    {ok, Channel} = amqp_connection:open_channel(Connection),
    Queue = <<"quasidock-durable-test">>,
    #'queue.declare_ok'{} = amqp_channel:call(Channel, #'queue.declare'{queue = Queue, durable = true}),
    case Mode of
        "publish" ->
            #'confirm.select_ok'{} = amqp_channel:call(Channel, #'confirm.select'{}),
            amqp_channel:cast(Channel, #'basic.publish'{routing_key = Queue},
                              #amqp_msg{props = #'P_basic'{delivery_mode = 2}, payload = <<"quasidock-message">>}),
            true = amqp_channel:wait_for_confirms(Channel),
            erlang:display(publish_ok);
        "consume" ->
            {#'basic.get_ok'{delivery_tag = Tag}, #amqp_msg{payload = <<"quasidock-message">>}} =
                amqp_channel:call(Channel, #'basic.get'{queue = Queue}),
            amqp_channel:cast(Channel, #'basic.ack'{delivery_tag = Tag}),
            erlang:display(consume_ok)
    end,
    ok = amqp_channel:close(Channel),
    ok = amqp_connection:close(Connection).
